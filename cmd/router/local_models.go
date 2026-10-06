package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"weave-os/router/internal/providers"
	openaiCompatProvider "weave-os/router/internal/providers/openaicompat"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"
)

// localModelsFileEnv names the YAML file declaring self-hosted models.
const localModelsFileEnv = "ROUTER_LOCAL_MODELS_FILE"

// maxLocalProviderNameLen matches the session-pin provider columns (VARCHAR(32)).
const maxLocalProviderNameLen = 32

var (
	errLocalModelMissingID      = errors.New("local model: id is required")
	errLocalModelInvalidID      = errors.New("local model: invalid id")
	errLocalModelDuplicateID    = errors.New("local model: duplicate id")
	errLocalModelMissingBaseURL = errors.New("local model: base_url is required")
	errLocalModelInvalidBaseURL = errors.New("local model: base_url must be an absolute http(s) URL")
	errLocalModelMissingKeyEnv  = errors.New("local model: api_key_env is required")
	errLocalModelKeyEnvUnset    = errors.New("local model: api_key_env names an unset environment variable")
	errLocalModelMissingUpstrm  = errors.New("local model: upstream_model is required")
	errLocalModelContextWindow  = errors.New("local model: context_window must be positive")
	errLocalModelInvalidField   = errors.New("local model: invalid field value")
)

// Lowercase because force-model input is lowercased before catalog lookup; no
// ':' because that introduces an effort suffix; no '/' so the derived provider
// name stays a flat identifier.
var localModelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type localModelsFile struct {
	Models []localModelEntry `yaml:"models"`
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
}

// localModel is one validated entry, ready to register.
type localModel struct {
	provider string
	baseURL  string
	apiKey   string
	keyEnv   string
	model    catalog.Model
}

// parseLocalModels decodes and validates a local-models file. Unknown keys are
// rejected so a misspelt field cannot silently fall back to a default.
func parseLocalModels(r io.Reader, getenv func(string) string) ([]localModel, error) {
	var file localModelsFile
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode local models: %w", err)
	}
	out := make([]localModel, 0, len(file.Models))
	seen := make(map[string]struct{}, len(file.Models))
	for i, entry := range file.Models {
		model, err := validateLocalModel(entry, getenv)
		if err != nil {
			return nil, fmt.Errorf("local model entry %d (%q): %w", i+1, entry.ID, err)
		}
		if _, dup := seen[entry.ID]; dup {
			return nil, fmt.Errorf("local model entry %d: %w: %s", i+1, errLocalModelDuplicateID, entry.ID)
		}
		seen[entry.ID] = struct{}{}
		out = append(out, model)
	}
	return out, nil
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
	return localModel{
		provider: provider,
		baseURL:  entry.BaseURL,
		apiKey:   apiKey,
		keyEnv:   entry.APIKeyEnv,
		model:    model,
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
			m.apiKey, m.baseURL, map[string]string{m.model.ID: m.model.Providers[0].UpstreamID})
		envKeyedProviders[m.provider] = struct{}{}
		logger.Info("Local model provider enabled",
			"provider", m.provider, "model", m.model.ID, "base_url", m.baseURL, "tier", m.model.Tier.String())
	}
	return nil
}

// loadLocalModels reads ROUTER_LOCAL_MODELS_FILE, when set, and registers its
// models. An unset variable registers nothing.
func loadLocalModels(
	getenv func(string) string,
	providerMap map[string]providers.Client,
	envKeyedProviders map[string]struct{},
	logger *slog.Logger,
) error {
	path := strings.TrimSpace(getenv(localModelsFileEnv))
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", localModelsFileEnv, err)
	}
	defer f.Close()
	models, err := parseLocalModels(f, getenv)
	if err != nil {
		return fmt.Errorf("%s: %w", localModelsFileEnv, err)
	}
	return registerLocalModels(models, providerMap, envKeyedProviders, logger)
}
