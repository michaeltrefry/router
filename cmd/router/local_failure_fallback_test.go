package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// failingLocal is a local OpenAI-compatible server that fails in one of the
// ways a self-hosted model goes down.
type failingLocal struct {
	mu       sync.Mutex
	requests int
	baseURL  string
}

func (f *failingLocal) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *failingLocal) handle(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	f.baseURL = srv.URL + "/v1"
}

const localChunk = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"partial local answer"},"finish_reason":null}]}` + "\n\n"

// newFailingLocal starts a local server failing with mode: "refused" (nothing
// listens), "500", "stall" (no response headers until the client gives up) or
// "midstream" (one content chunk, then the connection drops).
func newFailingLocal(t *testing.T, mode string) *failingLocal {
	t.Helper()
	f := &failingLocal{}
	switch mode {
	case "refused":
		srv := httptest.NewServer(http.NotFoundHandler())
		f.baseURL = srv.URL + "/v1"
		srv.Close()
	case "500":
		f.handle(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"model crashed"}}`)
		})
	case "stall":
		f.handle(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
	case "midstream":
		f.handle(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, localChunk)
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
	default:
		t.Fatalf("unknown failure mode %q", mode)
	}
	return f
}

// streamingAnthropic is the normal routing target: it streams a complete
// Anthropic message and records the model each dispatch asked for.
type streamingAnthropic struct {
	mu     sync.Mutex
	models []string
}

func (*streamingAnthropic) IncludedOnlySubscriptions() bool { return true }

func (a *streamingAnthropic) served() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.models...)
}

func (a *streamingAnthropic) Proxy(_ context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	a.mu.Lock()
	a.models = append(a.models, decision.Model)
	a.mu.Unlock()
	if !gjson.GetBytes(prep.Body, "stream").Bool() {
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":"msg_n","type":"message","role":"assistant","model":"`+decision.Model+`","content":[{"type":"text","text":"normal route answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, err := io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_n\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+decision.Model+"\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"+
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"normal route answer\"}}\n\n"+
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"+
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return err
}

func (*streamingAnthropic) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

// countingRouter scores every turn onto one model and counts its calls.
type countingRouter struct {
	mu       sync.Mutex
	decision router.Decision
	calls    int
}

func (r *countingRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.decision, nil
}

func (r *countingRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// localFailureService boots the local-models file through the composition
// root next to an Anthropic normal target; extraYAML extends the model entry
// or adds top-level blocks.
func localFailureService(t *testing.T, id string, local *failingLocal, scorer *countingRouter, extraYAML string) (*proxy.Service, *streamingAnthropic) {
	t.Helper()
	path := writeLocalModelsFile(t, localEntryYAML(id, local.baseURL, "LOCAL_TEST_KEY")+extraYAML)
	anthropicClient := &streamingAnthropic{}
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
	svc := proxy.NewService(scorer, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithLocalTurnRoute(cfg.turnRoute).
		WithMidTierSubstitute(cfg.midTier)
	return svc, anthropicClient
}

func loggedCtx(logs *bytes.Buffer) context.Context {
	return observability.WithLogger(routerKeyedCtx(), slog.New(slog.NewJSONHandler(logs, nil)))
}

// logLine returns the first JSON log line whose message is msg.
func logLine(t *testing.T, logs *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, `"msg":"`+msg+`"`) {
			continue
		}
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &fields))
		return fields
	}
	t.Fatalf("no %q log line", msg)
	return nil
}

const (
	localTitleGenBody = `{"model":"claude-haiku-4-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"properties":{"title":{"type":"string"}}}}}}`
	localSubAgentBody = `{"model":"claude-opus-4-7","max_tokens":256,"stream":true,"metadata":{"user_id":"subagent:Explore"},` +
		`"messages":[{"role":"user","content":"list the go files"}]}`
	localHeaderTimeoutYAML = "    response_header_timeout: 150ms\n"
)

var sonnet5Scorer = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-5", Reason: "cluster"}

// A title turn the local turn route took is served by the utility hard pin it
// would have had without the route, whichever way the local server fails.
func TestLocalFailure_TurnRouteTitleFallsBackToHardPin(t *testing.T) {
	for _, mode := range []string{"refused", "500", "stall"} {
		t.Run(mode, func(t *testing.T) {
			id := "test-lf-title-" + mode
			local := newFailingLocal(t, mode)
			scorer := &countingRouter{decision: sonnet5Scorer}
			svc, anthropicClient := localFailureService(t, id, local, scorer, localHeaderTimeoutYAML+turnRoutingYAML(id))
			var logs bytes.Buffer
			rec := httptest.NewRecorder()

			start := time.Now()
			require.NoError(t, svc.ProxyMessages(loggedCtx(&logs), []byte(localTitleGenBody), rec, claudeCodeRequest("")))

			assert.Less(t, time.Since(start), 4*time.Second, "a down local server is abandoned within its header timeout")
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, []string{"claude-haiku-4-5"}, anthropicClient.served(), "the turn's hard pin serves it")
			assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
			events := parseClientSSE(t, rec.Body.String())
			assert.Equal(t, "message_stop", events[len(events)-1].name)
			assert.Contains(t, rec.Body.String(), "normal route answer")
			assert.NotContains(t, rec.Body.String(), "Weave Router", "a title turn never carries a badge")
			assert.Zero(t, scorer.count(), "a hard-pinned title turn never consults the scorer")
			if mode != "refused" {
				assert.Positive(t, local.count(), "the local model is tried first")
			}

			line := logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
			assert.Equal(t, id, line["local_model"])
			assert.Equal(t, "local_turn_route", line["local_source"])
			assert.Equal(t, "claude-haiku-4-5", line["fallback_model"])
			done := logLine(t, &logs, "ProxyMessages complete")
			assert.Equal(t, true, done["local_failure_fallback"])
			assert.Equal(t, "claude-haiku-4-5", done["decision_model"])
		})
	}
}

// A sub-agent turn the local route took re-routes as it would have without
// the route: the scorer runs only once the local model has failed.
func TestLocalFailure_TurnRouteSubAgentFallsBackToScorer(t *testing.T) {
	const id = "test-lf-subagent"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster"}}
	svc, anthropicClient := localFailureService(t, id, local, scorer, turnRoutingYAML(id))
	var logs bytes.Buffer
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyMessages(loggedCtx(&logs), []byte(localSubAgentBody), rec, claudeCodeRequest("")))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, local.count())
	assert.Equal(t, 1, scorer.count(), "normal routing is computed once, on failure")
	assert.Equal(t, []string{"claude-opus-4-7"}, anthropicClient.served())
	assert.Contains(t, rec.Body.String(), "normal route answer")
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "local "+id+" failed"), "the badge names the failed local model once")
	assert.Contains(t, rec.Body.String(), "→ claude-opus-4-7 · local "+id+" failed")
}

// A mid-tier substitute that fails is replaced by the router's own pick.
func TestLocalFailure_MidTierSubstituteFallsBackToOriginalPick(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := map[bool]string{true: "stream", false: "non-stream"}[stream]
		t.Run(name, func(t *testing.T) {
			id := "test-lf-mid-" + name
			local := newFailingLocal(t, "500")
			scorer := &countingRouter{decision: sonnet5Scorer}
			svc, anthropicClient := localFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
			body := localSubstituteBody
			if !stream {
				// A distinct prompt keeps the stream case's session out of this one.
				body = strings.Replace(strings.Replace(body, `"stream":true`, `"stream":false`, 1), "failing build", "flaky test", 1)
			}
			var logs bytes.Buffer
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyMessages(loggedCtx(&logs), []byte(body), rec, claudeCodeRequest("")))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Positive(t, local.count(), "the substitute is tried first")
			assert.Equal(t, 1, scorer.count(), "the original pick is reused, not re-scored")
			assert.Equal(t, []string{"claude-sonnet-5"}, anthropicClient.served())
			assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
			assert.Contains(t, rec.Body.String(), "normal route answer")
			if stream {
				assert.Equal(t, 1, strings.Count(rec.Body.String(), "→ claude-sonnet-5 · local "+id+" failed"))
			}
			line := logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
			assert.Equal(t, "mid_tier_substitute", line["local_source"])
			done := logLine(t, &logs, "ProxyMessages complete")
			assert.Equal(t, "claude-sonnet-5", done["decision_model"])
			assert.Empty(t, done["substituted_from_model"], "the turn was served by its original pick")
		})
	}
}

// Bytes already reached the client: the turn ends with a stream error and
// no second upstream request is made.
func TestLocalFailure_CommittedStreamIsNeverRetried(t *testing.T) {
	cases := map[string]string{
		"turn route":          turnRoutingYAML("%s"),
		"mid-tier substitute": "mid_tier_substitute:\n  model: %s\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			id := "test-lf-commit-" + strings.Fields(name)[0]
			local := newFailingLocal(t, "midstream")
			scorer := &countingRouter{decision: sonnet5Scorer}
			svc, anthropicClient := localFailureService(t, id, local, scorer, strings.ReplaceAll(block, "%s", id))
			body := localSubstituteBody
			if name == "turn route" {
				body = localSubAgentBody
			}
			rec := httptest.NewRecorder()

			err := svc.ProxyMessages(routerKeyedCtx(), []byte(body), rec, claudeCodeRequest(""))

			require.Error(t, err)
			assert.Equal(t, 1, local.count(), "exactly one upstream request")
			assert.Empty(t, anthropicClient.served(), "a committed stream is never re-served")
			out := rec.Body.String()
			assert.Contains(t, out, "partial local answer")
			assert.Contains(t, out, "event: error", "the client sees the stream fail")
		})
	}
}

// A user-forced local model is what the user asked for: its failure surfaces.
func TestLocalFailure_ForcedLocalModelSurfacesError(t *testing.T) {
	const id = "test-lf-forced"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: sonnet5Scorer}
	svc, anthropicClient := localFailureService(t, id, local, scorer, turnRoutingYAML(id)+"mid_tier_substitute:\n  model: "+id+"\n")
	rec := httptest.NewRecorder()

	err := svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), rec, claudeCodeRequest(id))

	require.Error(t, err)
	assert.Positive(t, local.count())
	assert.Empty(t, anthropicClient.served(), "a forced model never falls back")
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

func TestParseLocalModels_ResponseHeaderTimeout(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entry := localEntryYAML("m1", "http://localhost:1/v1", "KEY_A")

	cfg, err := parseLocalModels(strings.NewReader("models:\n"+entry+"    response_header_timeout: 15s\n"), env)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, cfg.models[0].headerTimeout)

	cfg, err = parseLocalModels(strings.NewReader("models:\n"+entry), env)
	require.NoError(t, err)
	assert.Zero(t, cfg.models[0].headerTimeout, "omitted keeps the client default")

	for _, bad := range []string{"soon", "0s", "-1s"} {
		_, err := parseLocalModels(strings.NewReader("models:\n"+entry+"    response_header_timeout: "+bad+"\n"), env)
		require.ErrorIs(t, err, errLocalModelInvalidField, bad)
	}
}
