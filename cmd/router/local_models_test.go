package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/turntype"
)

const localTestInstallationID = "00000000-0000-0000-0000-0000000000aa"

func envFrom(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func localEntryYAML(id, baseURL, keyEnv string) string {
	return "  - id: " + id + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key_env: " + keyEnv + "\n" +
		"    upstream_model: upstream-" + id + "\n" +
		"    context_window: 262144\n" +
		"    tier: mid\n"
}

func TestParseLocalModels_ExampleConfig(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "docs", "local-models.example.yaml"))
	require.NoError(t, err)
	defer f.Close()

	cfg, err := parseLocalModels(f, envFrom(map[string]string{"LOCAL_QWEN_API_KEY": "secret"}))

	require.NoError(t, err)
	require.Len(t, cfg.models, 1)
	m := cfg.models[0]
	assert.Equal(t, "local_qwen3.8-flash-next", m.provider)
	assert.Equal(t, "http://localhost:8081/v1", m.baseURL)
	assert.Equal(t, "secret", m.apiKey)
	assert.Equal(t, "qwen3.8-flash-next", m.model.ID)
	assert.Equal(t, catalog.TierMid, m.model.Tier)
	assert.Equal(t, 262_144, m.model.ContextWindow)
	assert.Equal(t, catalog.ToolUseUnknown, m.model.ToolUseQuality)
	assert.Equal(t, catalog.AgenticUnknown, m.model.AgenticUse)
	assert.Equal(t, catalog.ImageInputUnsupported, m.model.ImageInput)
	assert.False(t, m.model.ThinkTagReasoning)
	require.Len(t, m.model.Providers, 1)
	assert.Equal(t, "local_qwen3.8-flash-next", m.model.Providers[0].Provider)
	assert.Equal(t, "qwen3.8-flash-next-unsloth-ud-q4_k_xl", m.model.Providers[0].UpstreamID)
	assert.Zero(t, m.model.Providers[0].Price, "local models are free to serve")
	assert.Equal(t, proxy.LocalTurnRoute{
		Provider:  "local_qwen3.8-flash-next",
		Model:     "qwen3.8-flash-next",
		TurnTypes: proxy.DefaultLocalTurnTypes,
	}, cfg.turnRoute)
	assert.Equal(t, proxy.MidTierSubstitute{Provider: "local_qwen3.8-flash-next", Model: "qwen3.8-flash-next"}, cfg.midTier)
	assert.Equal(t, proxy.SubscriptionLocalFallback{Provider: "local_qwen3.8-flash-next", Model: "qwen3.8-flash-next"}, cfg.subscriptionFallback)
}

func TestParseLocalModels_TurnRouting(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := localEntryYAML("m1", "http://localhost:1/v1", "KEY_A")
	parse := func(t *testing.T, routing string) (localModelsConfig, error) {
		t.Helper()
		return parseLocalModels(strings.NewReader("models:\n"+entries+routing), env)
	}

	t.Run("omitted block routes nothing", func(t *testing.T) {
		cfg, err := parse(t, "")
		require.NoError(t, err)
		assert.Equal(t, proxy.LocalTurnRoute{}, cfg.turnRoute)
	})
	t.Run("omitted turn types select the defaults", func(t *testing.T) {
		cfg, err := parse(t, "turn_routing:\n  model: m1\n")
		require.NoError(t, err)
		assert.Equal(t, providers.LocalProviderName("m1"), cfg.turnRoute.Provider)
		assert.Equal(t, "m1", cfg.turnRoute.Model)
		assert.Equal(t, []turntype.TurnType{turntype.SubAgentDispatch, turntype.TitleGen, turntype.Probe, turntype.Recap}, cfg.turnRoute.TurnTypes)
	})
	t.Run("explicit turn types replace the defaults", func(t *testing.T) {
		cfg, err := parse(t, "turn_routing:\n  model: m1\n  turn_types: [title_gen]\n")
		require.NoError(t, err)
		assert.Equal(t, []turntype.TurnType{turntype.TitleGen}, cfg.turnRoute.TurnTypes)
	})
	rejected := []struct {
		name    string
		routing string
		want    error
	}{
		{"unconfigured model", "turn_routing:\n  model: claude-sonnet-4-6\n", errLocalTurnRoutingModel},
		{"missing model", "turn_routing:\n  turn_types: [title_gen]\n", errLocalTurnRoutingModel},
		{"classifier", "turn_routing:\n  model: m1\n  turn_types: [title_gen, classifier]\n", errLocalTurnRoutingType},
		{"compaction", "turn_routing:\n  model: m1\n  turn_types: [compaction]\n", errLocalTurnRoutingType},
		{"main loop", "turn_routing:\n  model: m1\n  turn_types: [main_loop]\n", errLocalTurnRoutingType},
		{"tool result", "turn_routing:\n  model: m1\n  turn_types: [tool_result]\n", errLocalTurnRoutingType},
		{"unknown type", "turn_routing:\n  model: m1\n  turn_types: [explore]\n", errLocalTurnRoutingType},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := parse(t, tc.routing)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestParseLocalModels_MidTierSubstitute(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") +
		strings.Replace(localEntryYAML("hi1", "http://localhost:2/v1", "KEY_A"), "tier: mid", "tier: high", 1)
	parse := func(t *testing.T, block string) (localModelsConfig, error) {
		t.Helper()
		return parseLocalModels(strings.NewReader("models:\n"+entries+block), env)
	}

	t.Run("omitted block substitutes nothing", func(t *testing.T) {
		cfg, err := parse(t, "")
		require.NoError(t, err)
		assert.Equal(t, proxy.MidTierSubstitute{}, cfg.midTier)
	})
	t.Run("block enables substitution by default", func(t *testing.T) {
		cfg, err := parse(t, "mid_tier_substitute:\n  model: m1\n")
		require.NoError(t, err)
		assert.Equal(t, proxy.MidTierSubstitute{Provider: providers.LocalProviderName("m1"), Model: "m1"}, cfg.midTier)
	})
	t.Run("enabled false turns it off", func(t *testing.T) {
		cfg, err := parse(t, "mid_tier_substitute:\n  model: m1\n  enabled: false\n")
		require.NoError(t, err)
		assert.Equal(t, proxy.MidTierSubstitute{}, cfg.midTier)
	})
	rejected := []struct {
		name  string
		block string
		want  error
	}{
		{"unconfigured model", "mid_tier_substitute:\n  model: claude-sonnet-5\n", errMidTierSubstituteModel},
		{"missing model", "mid_tier_substitute:\n  enabled: true\n", errMidTierSubstituteModel},
		{"non-mid model", "mid_tier_substitute:\n  model: hi1\n", errMidTierSubstituteTier},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := parse(t, tc.block)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestParseLocalModels_SubscriptionFallback(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") +
		strings.Replace(localEntryYAML("hi1", "http://localhost:2/v1", "KEY_A"), "tier: mid", "tier: high", 1)
	parse := func(t *testing.T, block string) (localModelsConfig, error) {
		t.Helper()
		return parseLocalModels(strings.NewReader("models:\n"+entries+block), env)
	}

	t.Run("omitted block falls back to nothing", func(t *testing.T) {
		cfg, err := parse(t, "")
		require.NoError(t, err)
		assert.Equal(t, proxy.SubscriptionLocalFallback{}, cfg.subscriptionFallback)
	})
	t.Run("block enables the fallback by default", func(t *testing.T) {
		cfg, err := parse(t, "subscription_fallback:\n  model: m1\n")
		require.NoError(t, err)
		assert.Equal(t, proxy.SubscriptionLocalFallback{Provider: providers.LocalProviderName("m1"), Model: "m1"}, cfg.subscriptionFallback)
	})
	t.Run("any tier may serve the fallback", func(t *testing.T) {
		cfg, err := parse(t, "subscription_fallback:\n  model: hi1\n")
		require.NoError(t, err)
		assert.Equal(t, "hi1", cfg.subscriptionFallback.Model)
	})
	t.Run("enabled false turns it off", func(t *testing.T) {
		cfg, err := parse(t, "subscription_fallback:\n  model: m1\n  enabled: false\n")
		require.NoError(t, err)
		assert.Equal(t, proxy.SubscriptionLocalFallback{}, cfg.subscriptionFallback)
	})
	rejected := map[string]string{
		"unconfigured model": "subscription_fallback:\n  model: claude-sonnet-5\n",
		"missing model":      "subscription_fallback:\n  enabled: true\n",
	}
	for name, block := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := parse(t, block)
			require.ErrorIs(t, err, errSubscriptionFallbackModel)
		})
	}
}

func TestParseLocalModels_RejectsInvalidEntries(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	cases := []struct {
		name string
		yaml string
		want error
	}{
		{"missing base url", localEntryYAML("m1", `""`, "KEY_A"), errLocalModelMissingBaseURL},
		{"unset api key env var", localEntryYAML("m1", "http://localhost:1/v1", "KEY_UNSET"), errLocalModelKeyEnvUnset},
		{"duplicate id", localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") + localEntryYAML("m1", "http://localhost:2/v1", "KEY_A"), errLocalModelDuplicateID},
		{"id shadowed by a force-model alias", localEntryYAML("qwen", "http://localhost:1/v1", "KEY_A"), errLocalModelInvalidID},
		{"uppercase id is unforceable", localEntryYAML("Qwen-Local", "http://localhost:1/v1", "KEY_A"), errLocalModelInvalidID},
		{"non-http base url", localEntryYAML("m1", "ftp://localhost/v1", "KEY_A"), errLocalModelInvalidBaseURL},
		{"unknown tier", strings.Replace(localEntryYAML("m1", "http://localhost:1/v1", "KEY_A"), "tier: mid", "tier: sonnet", 1), errLocalModelInvalidField},
		{"unknown reasoning format", localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") + "    reasoning_format: bogus\n", errLocalModelInvalidField},
		{"unknown tool use rating", localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") + "    tool_use: high\n", errLocalModelInvalidField},
		{"unknown agentic rating", localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") + "    agentic: high\n", errLocalModelInvalidField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLocalModels(strings.NewReader("models:\n"+tc.yaml), env)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestParseLocalModels_NonDefaultCapabilities(t *testing.T) {
	doc := "models:\n" + localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") +
		"    tool_use: low\n" +
		"    agentic: low\n" +
		"    image_input: true\n" +
		"    reasoning_format: think_tags\n"

	cfg, err := parseLocalModels(strings.NewReader(doc), envFrom(map[string]string{"KEY_A": "a"}))

	require.NoError(t, err)
	require.Len(t, cfg.models, 1)
	m := cfg.models[0].model
	assert.Equal(t, catalog.ToolUseLow, m.ToolUseQuality)
	assert.Equal(t, catalog.AgenticLow, m.AgenticUse)
	assert.NotEqual(t, catalog.ImageInputUnsupported, m.ImageInput)
	assert.True(t, m.ThinkTagReasoning)
}

func TestParseLocalModels_RejectsUnknownField(t *testing.T) {
	doc := "models:\n" + localEntryYAML("m1", "http://localhost:1/v1", "KEY_A") + "    base_ur1: typo\n"
	_, err := parseLocalModels(strings.NewReader(doc), envFrom(map[string]string{"KEY_A": "a"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_ur1")
}

func TestLoadLocalModels_RejectsCatalogIDCollision(t *testing.T) {
	path := writeLocalModelsFile(t, localEntryYAML("claude-sonnet-4-6", "http://localhost:1/v1", "KEY_A"))
	_, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path, "KEY_A": "a"}),
		map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.ErrorIs(t, err, catalog.ErrDuplicateModelID)
}

func TestLoadLocalModels_UnsetFileRegistersNothing(t *testing.T) {
	providerMap := map[string]providers.Client{}
	cfg, err := loadLocalModels(envFrom(nil), providerMap, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	assert.Empty(t, providerMap)
	assert.Empty(t, cfg.turnRoute.Model)
	assert.Empty(t, cfg.midTier.Model)
	assert.Empty(t, cfg.subscriptionFallback.Model)
}

func writeLocalModelsFile(t *testing.T, entries string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "local-models.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models:\n"+entries), 0o600))
	return path
}

// localUpstream is a fake OpenAI-compatible server recording each request.
type localUpstream struct {
	mu      sync.Mutex
	paths   []string
	bodies  [][]byte
	authz   []string
	server  *httptest.Server
	baseURL string
}

func newLocalUpstream(t *testing.T) *localUpstream {
	return newScriptedLocalUpstream(t,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`+"\n\n"+
			"data: [DONE]\n\n")
}

// newScriptedLocalUpstream answers the nth request with streams[n] verbatim
// (the last stream repeats once the script runs out).
func newScriptedLocalUpstream(t *testing.T, streams ...string) *localUpstream {
	u := &localUpstream{}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.bodies = append(u.bodies, body)
		u.authz = append(u.authz, r.Header.Get("Authorization"))
		n := min(len(u.bodies), len(streams)) - 1
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streams[n])
	}))
	t.Cleanup(u.server.Close)
	u.baseURL = u.server.URL + "/v1"
	return u
}

// recordingAnthropic stands in for the Anthropic client, recording the
// credential each dispatch resolved.
type recordingAnthropic struct {
	mu    sync.Mutex
	creds []*proxy.Credentials
}

func (*recordingAnthropic) SupportsSubscriptions() bool { return true }

func (a *recordingAnthropic) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	a.mu.Lock()
	a.creds = append(a.creds, proxy.CredentialsFromContext(ctx))
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, err := io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	return err
}

func (*recordingAnthropic) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

type unusedRouter struct{ calls int }

func (r *unusedRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.calls++
	return router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6"}, nil
}

// localModelService boots the local-model wiring the composition root runs
// (config file → provider, client, catalog row) next to an Anthropic client.
func localModelService(t *testing.T, id string, upstream *localUpstream) (*proxy.Service, *recordingAnthropic, *unusedRouter) {
	t.Helper()
	return localModelServiceWithTelemetry(t, id, upstream, nil)
}

func localModelServiceWithTelemetry(t *testing.T, id string, upstream *localUpstream, telemetry proxy.TelemetryRepository) (*proxy.Service, *recordingAnthropic, *unusedRouter) {
	t.Helper()
	path := writeLocalModelsFile(t, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"))
	anthropicClient := &recordingAnthropic{}
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropicClient}
	keyed := map[string]struct{}{}
	_, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	rtr := &unusedRouter{}
	svc := proxy.NewService(rtr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", telemetry).
		WithDeploymentKeyedProviders(keyed)
	return svc, anthropicClient, rtr
}

func routerKeyedCtx() context.Context {
	ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-1")
	return context.WithValue(ctx, proxy.InstallationIDContextKey{}, localTestInstallationID)
}

// claudeCodeRequest mirrors Claude Code on the router-key path: the
// subscription OAuth bearer rides in Authorization.
func claudeCodeRequest(forceModel string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-ant-oat01-test-subscription")
	r.Header.Set(proxy.ForceModelHeader, forceModel)
	return r
}

const localTestBody = `{"model":"claude-sonnet-4-6","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`

func TestLocalModel_ForceModelDispatchesToConfiguredUpstream(t *testing.T) {
	const id = "test-local-forced"
	upstream := newLocalUpstream(t)
	svc, anthropicClient, rtr := localModelService(t, id, upstream)

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localTestBody), rec, claudeCodeRequest(id)))

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 1, "the forced local model must be served by its own upstream")
	assert.Equal(t, "/v1/chat/completions", upstream.paths[0])
	assert.Equal(t, "upstream-"+id, gjson.GetBytes(upstream.bodies[0], "model").String())
	assert.Equal(t, "Bearer local-secret", upstream.authz[0], "the configured key authenticates, never the caller's subscription token")
	assert.Empty(t, anthropicClient.creds)
	assert.Zero(t, rtr.calls, "a forced model never consults the scorer")
	assert.Contains(t, rec.Body.String(), "event: message_start")
}

// A configured local model must not displace the caller's Claude
// subscription the way a BYOK gateway displaces every vendor.
func TestLocalModel_ClaudeSubscriptionStillEnrollsAnthropic(t *testing.T) {
	upstream := newLocalUpstream(t)
	svc, anthropicClient, _ := localModelService(t, "test-local-coexist", upstream)

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localTestBody), rec, claudeCodeRequest("claude-sonnet-4-6")))

	anthropicClient.mu.Lock()
	defer anthropicClient.mu.Unlock()
	require.Len(t, anthropicClient.creds, 1, "Anthropic must stay enrolled via the subscription bearer")
	require.NotNil(t, anthropicClient.creds[0])
	assert.True(t, anthropicClient.creds[0].OAuth, "the turn is served on the caller's subscription")
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	assert.Empty(t, upstream.bodies)
}

// An unkeyed OpenAI-surface caller's own bearer belongs to another upstream;
// a forced local model must still authenticate with its configured key.
func TestLocalModel_UnkeyedOpenAICallerKeyNeverReachesLocalUpstream(t *testing.T) {
	const id = "test-local-unkeyed"
	upstream := newLocalUpstream(t)
	svc, _, _ := localModelService(t, id, upstream)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-openai-test")
	r.Header.Set(proxy.ForceModelHeader, id)
	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, r))

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.authz, 1, "the forced local model must be served by its own upstream")
	assert.Equal(t, "Bearer local-secret", upstream.authz[0])
}

// The composition-root wiring serves a title-generation turn from the local
// upstream while a main-loop turn stays on normal routing.
func TestLocalModel_TurnRoutingDispatchesTitleGenLocally(t *testing.T) {
	const id = "test-local-turns"
	upstream := newLocalUpstream(t)
	path := writeLocalModelsFile(t, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY")+
		"turn_routing:\n  model: "+id+"\n")
	anthropicClient := &recordingAnthropic{}
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropicClient}
	keyed := map[string]struct{}{providers.ProviderAnthropic: {}}
	cfg, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	svc := proxy.NewService(&unusedRouter{}, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithLocalTurnRoute(cfg.turnRoute)

	titleGen := `{"model":"claude-haiku-4-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"properties":{"title":{"type":"string"}}}}}}`
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(titleGen), httptest.NewRecorder(), claudeCodeRequest("")))
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localTestBody), httptest.NewRecorder(), claudeCodeRequest("")))

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 1, "only the title-generation turn reaches the local upstream")
	assert.Equal(t, "upstream-"+id, gjson.GetBytes(upstream.bodies[0], "model").String())
	assert.Equal(t, "Bearer local-secret", upstream.authz[0])
	anthropicClient.mu.Lock()
	defer anthropicClient.mu.Unlock()
	assert.Len(t, anthropicClient.creds, 1, "the main-loop turn stays on normal routing")
}

// recordingTelemetry captures the telemetry rows the dashboard metrics
// aggregate; every other repository method is unused here.
type recordingTelemetry struct {
	proxy.TelemetryRepository
	mu   sync.Mutex
	rows []proxy.InsertTelemetryParams
}

func (r *recordingTelemetry) InsertRequestTelemetry(_ context.Context, p proxy.InsertTelemetryParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, p)
	return nil
}

func (r *recordingTelemetry) snapshot() []proxy.InsertTelemetryParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proxy.InsertTelemetryParams(nil), r.rows...)
}

// localMainLoopBody carries a tool registry, as a real Claude Code turn does;
// a tool-less short request is a classifier turn, which never shows a marker.
const localMainLoopBody = `{"model":"claude-sonnet-4-6","max_tokens":8192,"stream":true,"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":"hello"}]}`

func TestLocalModel_ForcedTurnRecordsZeroCostAndNamesLocalModel(t *testing.T) {
	const id = "test-local-metrics"
	upstream := newLocalUpstream(t)
	tel := &recordingTelemetry{}
	svc, _, _ := localModelServiceWithTelemetry(t, id, upstream, tel)
	var logs bytes.Buffer
	ctx := observability.WithLogger(routerKeyedCtx(), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(ctx, []byte(localMainLoopBody), rec, claudeCodeRequest(id)))

	assert.Contains(t, rec.Body.String(), "→ "+id+" (local)", "the routing marker names the model and its local source")

	require.Eventually(t, func() bool { return len(tel.snapshot()) == 1 }, 2*time.Second, 10*time.Millisecond)
	row := tel.snapshot()[0]
	assert.Equal(t, id, row.DecisionModel)
	assert.Equal(t, providers.LocalProviderName(id), row.DecisionProvider)
	assert.Positive(t, row.InputTokens, "the turn's usage is recorded")
	assert.Positive(t, row.RequestedInputCostUSD, "the requested baseline is still priced")
	assert.Zero(t, row.ActualInputCostUSD+row.ActualOutputCostUSD, "a local turn costs $0")

	var complete map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		if entry["msg"] == "ProxyMessages complete" {
			complete = entry
		}
	}
	require.NotNil(t, complete, "the decision log line is emitted")
	assert.Equal(t, id, complete["decision_model"])
	assert.Equal(t, providers.LocalProviderName(id), complete["decision_provider"])
	assert.Contains(t, complete["routing_marker"], id+" (local)")
}

func TestLocalModel_ExcludedLocalModelIsNotServed(t *testing.T) {
	const id = "test-local-excluded"
	upstream := newLocalUpstream(t)
	svc, _, _ := localModelService(t, id, upstream)
	ctx := context.WithValue(routerKeyedCtx(), proxy.InstallationExcludedModelsContextKey{}, []string{id})

	rec := httptest.NewRecorder()
	err := svc.ProxyMessages(ctx, []byte(localTestBody), rec, claudeCodeRequest(id))

	require.ErrorIs(t, err, proxy.ErrForcedModelExcluded, "a dashboard-excluded local model is refused, not served")
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	assert.Empty(t, upstream.bodies, "the excluded local upstream is never called")
}

const localSubstituteBody = `{"model":"claude-sonnet-4-6","max_tokens":256,"stream":true,"system":"You are Claude Code.",` +
	`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":"fix the failing build in this repository"}]}`

type fixedRouter struct{ decision router.Decision }

func (r fixedRouter) Route(context.Context, router.Request) (router.Decision, error) {
	return r.decision, nil
}

// The composition-root wiring serves a main-loop turn the scorer gave to
// claude-sonnet-5 from the configured mid-tier substitute's upstream.
func TestLocalModel_MidTierSubstituteServesSonnetSelectionLocally(t *testing.T) {
	const id = "test-local-mid"
	upstream := newLocalUpstream(t)
	path := writeLocalModelsFile(t, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY")+
		"mid_tier_substitute:\n  model: "+id+"\n")
	anthropicClient := &recordingAnthropic{}
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropicClient}
	keyed := map[string]struct{}{providers.ProviderAnthropic: {}}
	cfg, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	scorer := fixedRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-5", Reason: "cluster"}}
	svc := proxy.NewService(scorer, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithMidTierSubstitute(cfg.midTier)

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), rec, claudeCodeRequest("")))

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 1, "the main-loop turn reaches the local upstream")
	assert.Equal(t, "upstream-"+id, gjson.GetBytes(upstream.bodies[0], "model").String())
	assert.Equal(t, "Bearer local-secret", upstream.authz[0])
	assert.Equal(t, id, rec.Header().Get(proxy.HeaderRouterModel))
	anthropicClient.mu.Lock()
	defer anthropicClient.mu.Unlock()
	assert.Empty(t, anthropicClient.creds)
}
