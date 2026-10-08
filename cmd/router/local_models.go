package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"weave-os/router/internal/providers"
	openaiCompatProvider "weave-os/router/internal/providers/openaicompat"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/turntype"
)

// localModelsFileEnv names the YAML file declaring self-hosted models.
const localModelsFileEnv = "ROUTER_LOCAL_MODELS_FILE"

// maxLocalProviderNameLen matches the session-pin provider columns (VARCHAR(32)).
const maxLocalProviderNameLen = 32

var (
	errLocalModelMissingID       = errors.New("local model: id is required")
	errLocalModelInvalidID       = errors.New("local model: invalid id")
	errLocalModelDuplicateID     = errors.New("local model: duplicate id")
	errLocalModelMissingBaseURL  = errors.New("local model: base_url is required")
	errLocalModelInvalidBaseURL  = errors.New("local model: base_url must be an absolute http(s) URL")
	errLocalModelMissingKeyEnv   = errors.New("local model: api_key_env is required")
	errLocalModelKeyEnvUnset     = errors.New("local model: api_key_env names an unset environment variable")
	errLocalModelMissingUpstrm   = errors.New("local model: upstream_model is required")
	errLocalModelContextWindow   = errors.New("local model: context_window must be positive")
	errLocalModelInvalidField    = errors.New("local model: invalid field value")
	errLocalTurnRoutingModel     = errors.New("local turn routing: model must name a configured local model")
	errLocalTurnRoutingType      = errors.New("local turn routing: turn type cannot be served locally")
	errMidTierSubstituteModel    = errors.New("mid-tier substitute: model must name a configured local model")
	errMidTierSubstituteTier     = errors.New("mid-tier substitute: model must be tier mid")
	errSubscriptionFallbackModel = errors.New("subscription fallback: model must name a configured local model")
	errModelMappingSource        = errors.New("model mapping: source must be a catalog model")
	errModelMappingTarget        = errors.New("model mapping: target must be a catalog model")
	errModelMappingChain         = errors.New("model mapping: target must not itself be mapped")
	errModelMappingUnselectable  = errors.New("model mapping: source is never selected by the active cluster scorer")
	errSubstitutionRuleModel     = errors.New("substitution rule: model must name a configured local model")
	errSubstitutionRuleGlob      = errors.New("substitution rule: match is not a valid glob pattern")
	errSubstitutionRuleNoMatch   = errors.New("substitution rule: match names no catalog model")
)

// Lowercase because force-model input is lowercased before catalog lookup; no
// ':' because that introduces an effort suffix; no '/' so the derived provider
// name stays a flat identifier.
var localModelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type localModelsFile struct {
	Models            []localModelEntry            `yaml:"models"`
	TurnRouting       *localTurnRoutingEntry       `yaml:"turn_routing"`
	MidTierSubstitute *localMidTierSubstituteEntry `yaml:"mid_tier_substitute"`
	// SubscriptionFallback shares the substitute's shape: a model and an
	// enabled flag that defaults to true.
	SubscriptionFallback *localMidTierSubstituteEntry `yaml:"subscription_fallback"`
	// ModelMapping maps an automatically selected model to the model served.
	ModelMapping map[string]string `yaml:"model_mapping"`
	// SubstitutionRules serve a (mapped) automatic selection matching a
	// model pattern on a local model; the first match wins.
	SubstitutionRules []localSubstitutionRuleEntry `yaml:"substitution_rules"`
}

// localSubstitutionRuleEntry serves selections whose model matches Match, a
// model ID or glob pattern, on the configured local model Model.
type localSubstitutionRuleEntry struct {
	Match string `yaml:"match"`
	Model string `yaml:"model"`
}

// localMidTierSubstituteEntry names the local model that replaces automatic
// mid-tier selections. Enabled defaults to true so the block alone turns it on.
type localMidTierSubstituteEntry struct {
	Model   string `yaml:"model"`
	Enabled *bool  `yaml:"enabled"`
}

type localTurnRoutingEntry struct {
	Model     string   `yaml:"model"`
	TurnTypes []string `yaml:"turn_types"`
}

// localModelsConfig is a validated local-models file. A zero turnRoute
// leaves every turn type on normal routing; a zero midTier substitutes
// nothing; a zero subscriptionFallback leaves subscription refusals as they
// are; an empty modelMapping serves every selection as chosen; empty
// substitutionRules substitute nothing by model pattern.
type localModelsConfig struct {
	models               []localModel
	turnRoute            proxy.LocalTurnRoute
	midTier              proxy.MidTierSubstitute
	subscriptionFallback proxy.SubscriptionLocalFallback
	modelMapping         proxy.ModelMapping
	substitutionRules    []proxy.SubstitutionRule
}

type localModelEntry struct {
	ID              string `yaml:"id"`
	BaseURL         string `yaml:"base_url"`
	APIKeyEnv       string `yaml:"api_key_env"`
	UpstreamModel   string `yaml:"upstream_model"`
	ContextWindow   int    `yaml:"context_window"`
	Tier            string `yaml:"tier"`
	ToolUse         string `yaml:"tool_use"`
	Agentic         string `yaml:"agentic"`
	ImageInput      bool   `yaml:"image_input"`
	ReasoningFormat string `yaml:"reasoning_format"`
	// ResponseHeaderTimeout is a Go duration ("15s"); empty keeps the
	// openaicompat default.
	ResponseHeaderTimeout string `yaml:"response_header_timeout"`
}

// localModel is one validated entry, ready to register.
type localModel struct {
	provider string
	baseURL  string
	apiKey   string
	keyEnv   string
	model    catalog.Model
	// headerTimeout is zero when the entry keeps the client default.
	headerTimeout time.Duration
}

// parseLocalModels decodes and validates a local-models file. Unknown keys are
// rejected so a misspelt field cannot silently fall back to a default.
func parseLocalModels(r io.Reader, getenv func(string) string) (localModelsConfig, error) {
	var file localModelsFile
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return localModelsConfig{}, fmt.Errorf("decode local models: %w", err)
	}
	out := make([]localModel, 0, len(file.Models))
	seen := make(map[string]struct{}, len(file.Models))
	tiers := make(map[string]catalog.Tier, len(file.Models))
	for i, entry := range file.Models {
		model, err := validateLocalModel(entry, getenv)
		if err != nil {
			return localModelsConfig{}, fmt.Errorf("local model entry %d (%q): %w", i+1, entry.ID, err)
		}
		if _, dup := seen[entry.ID]; dup {
			return localModelsConfig{}, fmt.Errorf("local model entry %d: %w: %s", i+1, errLocalModelDuplicateID, entry.ID)
		}
		seen[entry.ID] = struct{}{}
		tiers[entry.ID] = model.model.Tier
		out = append(out, model)
	}
	route, err := validateLocalTurnRouting(file.TurnRouting, seen)
	if err != nil {
		return localModelsConfig{}, err
	}
	midTier, err := validateMidTierSubstitute(file.MidTierSubstitute, tiers)
	if err != nil {
		return localModelsConfig{}, err
	}
	fallback, err := validateSubscriptionFallback(file.SubscriptionFallback, seen)
	if err != nil {
		return localModelsConfig{}, err
	}
	mapping, err := validateModelMapping(file.ModelMapping)
	if err != nil {
		return localModelsConfig{}, err
	}
	rules, err := validateSubstitutionRules(file.SubstitutionRules, seen)
	if err != nil {
		return localModelsConfig{}, err
	}
	return localModelsConfig{
		models: out, turnRoute: route, midTier: midTier, subscriptionFallback: fallback,
		modelMapping: mapping, substitutionRules: rules,
	}, nil
}

// validateSubstitutionRules resolves the substitution_rules list in order.
// Each match must be a valid glob naming at least one built-in catalog model,
// so a misspelt ID cannot silently substitute nothing, and each model must be
// a configured local model of any tier.
func validateSubstitutionRules(entries []localSubstitutionRuleEntry, models map[string]struct{}) ([]proxy.SubstitutionRule, error) {
	rules := make([]proxy.SubstitutionRule, 0, len(entries))
	for i, entry := range entries {
		if _, configured := models[entry.Model]; !configured {
			return nil, fmt.Errorf("substitution rule %d: %w: %q", i+1, errSubstitutionRuleModel, entry.Model)
		}
		if _, err := path.Match(entry.Match, ""); err != nil {
			return nil, fmt.Errorf("substitution rule %d: %w: %q", i+1, errSubstitutionRuleGlob, entry.Match)
		}
		if !matchesCatalogModel(entry.Match) {
			return nil, fmt.Errorf("substitution rule %d: %w: %q", i+1, errSubstitutionRuleNoMatch, entry.Match)
		}
		rules = append(rules, proxy.SubstitutionRule{Match: entry.Match, Provider: providers.LocalProviderName(entry.Model), Model: entry.Model})
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return rules, nil
}

// matchesCatalogModel reports whether pattern matches a built-in catalog model.
func matchesCatalogModel(pattern string) bool {
	for _, m := range catalog.Models {
		if m.ID == "" || catalog.IsLocal(m.ID) {
			continue
		}
		if matched, _ := path.Match(pattern, m.ID); matched {
			return true
		}
	}
	return false
}

// validateModelMapping checks that both sides of every mapping are built-in
// catalog models and that no target is itself mapped, since a mapping is
// applied once. Local models are not catalog rows until registration, so a
// local target is rejected here.
func validateModelMapping(entries map[string]string) (proxy.ModelMapping, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	for _, source := range slices.Sorted(maps.Keys(entries)) {
		target := entries[source]
		if _, known := catalog.ByID(source); !known {
			return nil, fmt.Errorf("%w: %q", errModelMappingSource, source)
		}
		if _, known := catalog.ByID(target); !known || catalog.IsLocal(target) {
			return nil, fmt.Errorf("%w: %q maps to %q", errModelMappingTarget, source, target)
		}
		if _, mapped := entries[target]; mapped {
			return nil, fmt.Errorf("%w: %q maps to %q", errModelMappingChain, source, target)
		}
	}
	return proxy.ModelMapping(entries), nil
}

// validateModelMappingSelectable rejects a mapping whose source the active
// cluster scorer can never select, since such a mapping would never apply.
// candidates is that scorer's provider-filtered roster, built after
// TierMappingSources tiered the mapping's retired sources.
func validateModelMappingSelectable(mapping proxy.ModelMapping, version string, candidates []cluster.DeployedEntry) error {
	selectable := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		selectable[c.Model] = struct{}{}
	}
	for _, source := range slices.Sorted(maps.Keys(mapping)) {
		if _, ok := selectable[source]; !ok {
			return fmt.Errorf("%w: %q is not a candidate of cluster version %s", errModelMappingUnselectable, source, version)
		}
	}
	return nil
}

// validateSubscriptionFallback resolves the subscription_fallback block. Any
// tier is accepted; enabled: false keeps the block but falls back to nothing.
func validateSubscriptionFallback(entry *localMidTierSubstituteEntry, models map[string]struct{}) (proxy.SubscriptionLocalFallback, error) {
	if entry == nil {
		return proxy.SubscriptionLocalFallback{}, nil
	}
	if _, configured := models[entry.Model]; !configured {
		return proxy.SubscriptionLocalFallback{}, fmt.Errorf("%w: %q", errSubscriptionFallbackModel, entry.Model)
	}
	if entry.Enabled != nil && !*entry.Enabled {
		return proxy.SubscriptionLocalFallback{}, nil
	}
	return proxy.SubscriptionLocalFallback{Provider: providers.LocalProviderName(entry.Model), Model: entry.Model}, nil
}

// validateMidTierSubstitute resolves the mid_tier_substitute block. The model
// must be tier mid so a mid-tier pick is never served by a weaker or stronger
// class; enabled: false keeps the block but substitutes nothing.
func validateMidTierSubstitute(entry *localMidTierSubstituteEntry, tiers map[string]catalog.Tier) (proxy.MidTierSubstitute, error) {
	if entry == nil {
		return proxy.MidTierSubstitute{}, nil
	}
	tier, configured := tiers[entry.Model]
	if !configured {
		return proxy.MidTierSubstitute{}, fmt.Errorf("%w: %q", errMidTierSubstituteModel, entry.Model)
	}
	if tier != catalog.TierMid {
		return proxy.MidTierSubstitute{}, fmt.Errorf("%w: %q is tier %s", errMidTierSubstituteTier, entry.Model, tier.String())
	}
	if entry.Enabled != nil && !*entry.Enabled {
		return proxy.MidTierSubstitute{}, nil
	}
	return proxy.MidTierSubstitute{Provider: providers.LocalProviderName(entry.Model), Model: entry.Model}, nil
}

// validateLocalTurnRouting resolves the turn_routing block against the
// configured models. Omitted turn_types selects proxy.DefaultLocalTurnTypes.
func validateLocalTurnRouting(entry *localTurnRoutingEntry, models map[string]struct{}) (proxy.LocalTurnRoute, error) {
	if entry == nil {
		return proxy.LocalTurnRoute{}, nil
	}
	if _, configured := models[entry.Model]; !configured {
		return proxy.LocalTurnRoute{}, fmt.Errorf("%w: %q", errLocalTurnRoutingModel, entry.Model)
	}
	types := slices.Clone(proxy.DefaultLocalTurnTypes)
	if len(entry.TurnTypes) > 0 {
		types = make([]turntype.TurnType, 0, len(entry.TurnTypes))
		for _, raw := range entry.TurnTypes {
			tt := turntype.TurnType(raw)
			if !proxy.LocalTurnRoutable(tt) {
				return proxy.LocalTurnRoute{}, fmt.Errorf("%w: %q (want sub_agent_dispatch, title_gen, probe or recap)", errLocalTurnRoutingType, raw)
			}
			types = append(types, tt)
		}
	}
	return proxy.LocalTurnRoute{
		Provider:  providers.LocalProviderName(entry.Model),
		Model:     entry.Model,
		TurnTypes: types,
	}, nil
}

func validateLocalModel(entry localModelEntry, getenv func(string) string) (localModel, error) {
	if entry.ID == "" {
		return localModel{}, errLocalModelMissingID
	}
	provider := providers.LocalProviderName(entry.ID)
	if !localModelIDPattern.MatchString(entry.ID) || len(provider) > maxLocalProviderNameLen {
		return localModel{}, fmt.Errorf("%w: %q must match %s and fit a %d-character provider name", errLocalModelInvalidID, entry.ID, localModelIDPattern, maxLocalProviderNameLen)
	}
	if target, shadowed := proxy.ForceModelShadowTarget(entry.ID); shadowed {
		return localModel{}, fmt.Errorf("%w: %q already forces %s", errLocalModelInvalidID, entry.ID, target)
	}
	if entry.BaseURL == "" {
		return localModel{}, errLocalModelMissingBaseURL
	}
	parsed, err := url.Parse(entry.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return localModel{}, errLocalModelInvalidBaseURL
	}
	if entry.APIKeyEnv == "" {
		return localModel{}, errLocalModelMissingKeyEnv
	}
	apiKey := strings.TrimSpace(getenv(entry.APIKeyEnv))
	if apiKey == "" {
		return localModel{}, fmt.Errorf("%w: %s", errLocalModelKeyEnvUnset, entry.APIKeyEnv)
	}
	if entry.UpstreamModel == "" {
		return localModel{}, errLocalModelMissingUpstrm
	}
	if entry.ContextWindow <= 0 {
		return localModel{}, errLocalModelContextWindow
	}
	model := catalog.Model{
		ID: entry.ID,
		// Weights served from the operator's own hardware are published weights.
		Source:        catalog.SourceOpenSource,
		ContextWindow: entry.ContextWindow,
		Providers:     []catalog.ProviderBinding{{Provider: provider, UpstreamID: entry.UpstreamModel}},
	}
	if model.Tier, err = parseLocalTier(entry.Tier); err != nil {
		return localModel{}, err
	}
	if model.ToolUseQuality, err = parseLocalRating(entry.ToolUse, "tool_use", catalog.ToolUseUnknown, catalog.ToolUseLow); err != nil {
		return localModel{}, err
	}
	if model.AgenticUse, err = parseLocalRating(entry.Agentic, "agentic", catalog.AgenticUnknown, catalog.AgenticLow); err != nil {
		return localModel{}, err
	}
	if !entry.ImageInput {
		model.ImageInput = catalog.ImageInputUnsupported
	}
	switch entry.ReasoningFormat {
	case "", "reasoning_content":
	case "think_tags":
		model.ThinkTagReasoning = true
	default:
		return localModel{}, fmt.Errorf("%w: reasoning_format %q (want reasoning_content or think_tags)", errLocalModelInvalidField, entry.ReasoningFormat)
	}
	var headerTimeout time.Duration
	if entry.ResponseHeaderTimeout != "" {
		headerTimeout, err = time.ParseDuration(entry.ResponseHeaderTimeout)
		if err != nil || headerTimeout <= 0 {
			return localModel{}, fmt.Errorf("%w: response_header_timeout %q (want a positive duration such as 15s)", errLocalModelInvalidField, entry.ResponseHeaderTimeout)
		}
	}
	return localModel{
		provider:      provider,
		baseURL:       entry.BaseURL,
		apiKey:        apiKey,
		keyEnv:        entry.APIKeyEnv,
		model:         model,
		headerTimeout: headerTimeout,
	}, nil
}

func parseLocalTier(raw string) (catalog.Tier, error) {
	switch raw {
	case "low":
		return catalog.TierLow, nil
	case "mid":
		return catalog.TierMid, nil
	case "high":
		return catalog.TierHigh, nil
	default:
		return catalog.TierUnknown, fmt.Errorf("%w: tier %q (want low, mid or high)", errLocalModelInvalidField, raw)
	}
}

func parseLocalRating[T any](raw, field string, standard, low T) (T, error) {
	switch raw {
	case "", "default":
		return standard, nil
	case "low":
		return low, nil
	default:
		return standard, fmt.Errorf("%w: %s %q (want default or low)", errLocalModelInvalidField, field, raw)
	}
}

// registerLocalModels registers each local model's provider, OpenAI-compatible
// client and catalog row. Every local provider holds a deployment key, so it
// joins envKeyedProviders.
func registerLocalModels(
	models []localModel,
	providerMap map[string]providers.Client,
	envKeyedProviders map[string]struct{},
	logger *slog.Logger,
) error {
	rows := make([]catalog.Model, 0, len(models))
	for _, m := range models {
		if _, exists := providerMap[m.provider]; exists {
			return fmt.Errorf("%w: %s", providers.ErrProviderAlreadyRegistered, m.provider)
		}
		rows = append(rows, m.model)
	}
	if err := catalog.RegisterLocalModels(rows...); err != nil {
		return err
	}
	for _, m := range models {
		if err := providers.RegisterLocalProvider(m.provider, m.keyEnv); err != nil {
			return err
		}
		providerMap[m.provider] = openaiCompatProvider.NewClientWithModelIDMap(
			m.apiKey, m.baseURL, map[string]string{m.model.ID: m.model.Providers[0].UpstreamID},
			openaiCompatProvider.WithResponseHeaderTimeout(m.headerTimeout),
			openaiCompatProvider.WithPrivateBaseURL())
		envKeyedProviders[m.provider] = struct{}{}
		logger.Info("Local model provider enabled",
			"provider", m.provider, "model", m.model.ID, "base_url", m.baseURL, "tier", m.model.Tier.String())
	}
	return nil
}

// loadLocalModels reads ROUTER_LOCAL_MODELS_FILE, when set, registers its
// models and returns the validated file. An unset variable registers nothing.
func loadLocalModels(
	getenv func(string) string,
	providerMap map[string]providers.Client,
	envKeyedProviders map[string]struct{},
	logger *slog.Logger,
) (localModelsConfig, error) {
	path := strings.TrimSpace(getenv(localModelsFileEnv))
	if path == "" {
		return localModelsConfig{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return localModelsConfig{}, fmt.Errorf("%s: %w", localModelsFileEnv, err)
	}
	defer f.Close()
	cfg, err := parseLocalModels(f, getenv)
	if err != nil {
		return localModelsConfig{}, fmt.Errorf("%s: %w", localModelsFileEnv, err)
	}
	if err := registerLocalModels(cfg.models, providerMap, envKeyedProviders, logger); err != nil {
		return localModelsConfig{}, err
	}
	if err := catalog.TierMappingSources(cfg.modelMapping); err != nil {
		return localModelsConfig{}, err
	}
	if cfg.turnRoute.Model != "" {
		logger.Info("Local turn routing enabled", "model", cfg.turnRoute.Model, "turn_types", cfg.turnRoute.TurnTypes)
	}
	if cfg.midTier.Model != "" {
		logger.Info("Mid-tier local substitution enabled", "model", cfg.midTier.Model)
	}
	if cfg.subscriptionFallback.Model != "" {
		logger.Info("Subscription local fallback enabled", "model", cfg.subscriptionFallback.Model)
	}
	if len(cfg.modelMapping) > 0 {
		logger.Info("Model mapping enabled", "mappings", cfg.modelMapping)
	}
	for _, rule := range cfg.substitutionRules {
		logger.Info("Substitution rule enabled", "match", rule.Match, "model", rule.Model)
	}
	return cfg, nil
}
