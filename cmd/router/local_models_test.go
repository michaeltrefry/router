package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
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

	models, err := parseLocalModels(f, envFrom(map[string]string{"LOCAL_QWEN_API_KEY": "secret"}))

	require.NoError(t, err)
	require.Len(t, models, 1)
	m := models[0]
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

	models, err := parseLocalModels(strings.NewReader(doc), envFrom(map[string]string{"KEY_A": "a"}))

	require.NoError(t, err)
	require.Len(t, models, 1)
	m := models[0].model
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
	err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path, "KEY_A": "a"}),
		map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.ErrorIs(t, err, catalog.ErrDuplicateModelID)
}

func TestLoadLocalModels_UnsetFileRegistersNothing(t *testing.T) {
	providerMap := map[string]providers.Client{}
	require.NoError(t, loadLocalModels(envFrom(nil), providerMap, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	assert.Empty(t, providerMap)
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

func (*recordingAnthropic) IncludedOnlySubscriptions() bool { return true }

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
	path := writeLocalModelsFile(t, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"))
	anthropicClient := &recordingAnthropic{}
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropicClient}
	keyed := map[string]struct{}{}
	require.NoError(t, loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	rtr := &unusedRouter{}
	svc := proxy.NewService(rtr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
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
