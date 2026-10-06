package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/requestcontext"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamingOpenAI is the Codex normal routing target: it streams native
// Responses events, or Chat Completions chunks on that endpoint.
type streamingOpenAI struct {
	mu     sync.Mutex
	models []string
	// oauth and endpoints record, per dispatch, whether it went out on a
	// ChatGPT plan credential and which OpenAI surface it targeted.
	oauth     []bool
	endpoints []providers.Endpoint
	// oauthStatus, when set, rejects every dispatch on a plan credential.
	oauthStatus int
}

func (o *streamingOpenAI) calls() ([]bool, []providers.Endpoint) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]bool(nil), o.oauth...), append([]providers.Endpoint(nil), o.endpoints...)
}

func (*streamingOpenAI) IncludedOnlySubscriptions() bool { return true }

func (o *streamingOpenAI) served() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.models...)
}

func (o *streamingOpenAI) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	creds := proxy.CredentialsFromContext(ctx)
	oauth := creds != nil && creds.OAuth
	o.mu.Lock()
	o.models = append(o.models, decision.Model)
	o.oauth = append(o.oauth, oauth)
	o.endpoints = append(o.endpoints, prep.Endpoint)
	o.mu.Unlock()
	if oauth && o.oauthStatus != 0 {
		return &providers.UpstreamErrorResponse{
			Status:  o.oauthStatus,
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    []byte(`{"error":{"type":"usage_limit_reached","message":"plan limit reached"}}`),
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if prep.Endpoint != providers.EndpointResponses {
		_, err := io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"normal route answer"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`+"\n\n"+
			"data: [DONE]\n\n")
		return err
	}
	item := `{"id":"msg_n","type":"message","status":"in_progress","role":"assistant","content":[]}`
	done := `{"id":"msg_n","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"normal route answer","annotations":[]}]}`
	_, err := io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_n\",\"status\":\"in_progress\",\"output\":[]}}\n\n"+
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":"+item+"}\n\n"+
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"item_id\":\"msg_n\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n"+
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_n\",\"output_index\":0,\"content_index\":0,\"delta\":\"normal route answer\"}\n\n"+
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":4,\"output_index\":0,\"item\":"+done+"}\n\n"+
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_n\",\"status\":\"completed\",\"output\":["+done+"],\"usage\":{\"input_tokens\":5,\"output_tokens\":3,\"total_tokens\":8}}}\n\n")
	return err
}

func (*streamingOpenAI) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

func codexLocalFailureService(t *testing.T, id string, local *failingLocal, scorer *countingRouter, extraYAML string) (*proxy.Service, *streamingOpenAI) {
	t.Helper()
	return codexLocalFailureServiceWithPins(t, id, local, scorer, extraYAML, nil)
}

func codexLocalFailureServiceWithPins(t *testing.T, id string, local *failingLocal, scorer *countingRouter, extraYAML string, pins sessionpin.Store) (*proxy.Service, *streamingOpenAI) {
	t.Helper()
	path := writeLocalModelsFile(t, localEntryYAML(id, local.baseURL, "LOCAL_TEST_KEY")+extraYAML)
	openAIClient := &streamingOpenAI{}
	providerMap := map[string]providers.Client{providers.ProviderOpenAI: openAIClient}
	keyed := map[string]struct{}{providers.ProviderOpenAI: {}}
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
	svc := proxy.NewService(scorer, providerMap, nil, false, nil, pins, false, providers.ProviderOpenAI, "gpt-5.6-luna", nil).
		WithDeploymentKeyedProviders(keyed).
		WithLocalTurnRoute(cfg.turnRoute).
		WithMidTierSubstitute(cfg.midTier)
	return svc, openAIClient
}

// badgeCount counts rendered routing badges in a Responses stream's text deltas.
func badgeCount(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"response.output_text.delta"`) {
			n += strings.Count(line, "Weave Router")
		}
	}
	return n
}

// On a streamed Codex turn the local model's failure leaves no badge behind:
// the client sees one badge, naming the model that served.
func TestLocalFailure_CodexResponsesStreamFallsBackWithOneBadge(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		yaml    string
		scorer  router.Decision
		headers map[string]string
		served  string
	}{
		{
			name:    "spawned sub-agent on the turn route",
			id:      "test-lf-codex-sub",
			yaml:    "turn_routing:\n  model: %s\n",
			scorer:  router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-sol", Reason: "scored"},
			headers: map[string]string{"x-openai-subagent": "collab_spawn"},
			served:  "gpt-5.6-sol",
		},
		{
			name:   "mid-tier substitute",
			id:     "test-lf-codex-mid",
			yaml:   "mid_tier_substitute:\n  model: %s\n",
			scorer: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"},
			served: "gpt-5.6-luna",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := newFailingLocal(t, "500")
			scorer := &countingRouter{decision: tc.scorer}
			svc, openAIClient := codexLocalFailureService(t, tc.id, local, scorer, strings.ReplaceAll(tc.yaml, "%s", tc.id))
			ctx, r := codexRequest(t, tc.headers)
			var logs bytes.Buffer
			ctx = observability.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), rec, r))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Positive(t, local.count(), "the local model is tried first")
			assert.Equal(t, []string{tc.served}, openAIClient.served())
			out := rec.Body.String()
			events := parseClientSSE(t, out)
			assert.Len(t, responsesItems(t, events), 1)
			assert.Equal(t, "response.completed", events[len(events)-1].name)
			assert.Equal(t, 1, strings.Count(out, "event: response.created"))
			assert.Contains(t, out, "normal route answer")
			assert.Equal(t, 1, badgeCount(out), "exactly one badge reaches the client")
			assert.Contains(t, out, "→ "+tc.served+" · best pick for this turn · local "+tc.id+" failed")
			assert.NotContains(t, out, "(local)", "the failed local model's own badge never renders")
			logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
		})
	}
}

// A Codex stream that already reached the client is not re-served.
func TestLocalFailure_CodexCommittedStreamIsNeverRetried(t *testing.T) {
	const id = "test-lf-codex-commit"
	local := newFailingLocal(t, "midstream")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
	ctx, r := codexRequest(t, nil)
	rec := httptest.NewRecorder()

	_ = svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), rec, r)

	assert.Equal(t, 1, local.count(), "exactly one upstream request")
	assert.Empty(t, openAIClient.served(), "a committed stream is never re-served")
	out := rec.Body.String()
	assert.Contains(t, out, "partial local answer")
	assert.True(t, strings.Contains(out, "event: response.failed") || strings.Contains(out, "event: error"), "the client sees the stream fail: %s", out)
}

// The normal target that takes over a failed Codex turn keeps the paid
// rescue it would have had without local rules: its plan refusal is retried
// on the deployment key.
func TestLocalFailure_CodexNormalTargetPlanRefusalRetriesOnPaidKey(t *testing.T) {
	const id = "test-lf-codex-plan"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
	openAIClient.oauthStatus = http.StatusTooManyRequests
	ctx, r := codexRequest(t, nil)
	body := strings.Replace(codexMainTurn, "Explore the repository layout.", "Summarize the open issues.", 1)
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(body), rec, r))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, local.count(), "the substitute is tried first")
	assert.Equal(t, []string{"gpt-5.6-luna", "gpt-5.6-luna"}, openAIClient.served())
	oauth, _ := openAIClient.calls()
	assert.Equal(t, []bool{true, false}, oauth, "the plan refusal is retried on the deployment key")
	assert.Contains(t, rec.Body.String(), "normal route answer")
	assert.Contains(t, rec.Body.String(), "event: response.completed")
}

// A Codex turn on an API key whose normal target is a reasoning model with
// tools is re-served on the Responses API, as it would be without local rules.
func TestLocalFailure_CodexAPIKeyRescueUsesResponsesEndpoint(t *testing.T) {
	const id = "test-lf-codex-endpoint"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
	ctx, r := codexRequest(t, nil)
	r.Header.Del("Authorization")
	r.Header.Del(requestcontext.ChatGPTAccountIDHeader)
	body := strings.Replace(codexMainTurn, "Explore the repository layout.", "Rename the config loader.", 1)
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(body), rec, r))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"gpt-5.6-luna"}, openAIClient.served())
	oauth, endpoints := openAIClient.calls()
	assert.Equal(t, []bool{false}, oauth, "the turn runs on the deployment key")
	assert.Equal(t, []providers.Endpoint{providers.EndpointResponses}, endpoints)
	assert.Contains(t, rec.Body.String(), "normal route answer")
}

// When the normal route's badge is hidden (the session already sees that
// model), the failed local model's badge is cleared rather than left to
// render over the normal route's output.
func TestLocalFailure_CodexRescueClearsLocalBadgeWhenNormalBadgeHidden(t *testing.T) {
	const id = "test-lf-codex-clear"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureServiceWithPins(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n", &memoryPins{pins: map[string]sessionpin.Pin{}})
	body := strings.Replace(codexMainTurn, "Explore the repository layout.", "Tidy the imports.", 1)

	var outs []string
	for range 2 {
		ctx, r := codexRequest(t, nil)
		rec := httptest.NewRecorder()
		require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(body), rec, r))
		require.Equal(t, http.StatusOK, rec.Code)
		outs = append(outs, rec.Body.String())
	}

	assert.Equal(t, []string{"gpt-5.6-luna", "gpt-5.6-luna"}, openAIClient.served())
	assert.Equal(t, 1, badgeCount(outs[0]), "the first turn names the model that served")
	assert.Zero(t, badgeCount(outs[1]), "the session already sees the normal target: no badge")
	assert.NotContains(t, outs[1], "(local)", "the failed local model's badge never renders")
	assert.Contains(t, outs[1], "normal route answer")
}

const chatCompletionsTurn = `{"model":"gpt-5.6-sol","stream":true,` +
	`"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"system","content":"You are a coding agent."},{"role":"user","content":"Find the flaky test."}]}`

func chatCompletionsRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
}

// A plain Chat Completions caller gets the same fallback: a local failure
// before output is served by the normal target.
func TestLocalFailure_ChatCompletionsStreamFallsBack(t *testing.T) {
	const id = "test-lf-chat"
	local := newFailingLocal(t, "500")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyOpenAIChatCompletion(routerKeyedCtx(), []byte(chatCompletionsTurn), rec, chatCompletionsRequest()))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, local.count(), "the substitute is tried first")
	assert.Equal(t, []string{"gpt-5.6-luna"}, openAIClient.served())
	out := rec.Body.String()
	assert.Contains(t, out, "normal route answer")
	assert.Contains(t, out, "data: [DONE]")
	assert.NotContains(t, out, "partial local answer")
}

// A Chat Completions stream that already reached the client is not re-served.
func TestLocalFailure_ChatCompletionsCommittedStreamIsNeverRetried(t *testing.T) {
	const id = "test-lf-chat-commit"
	local := newFailingLocal(t, "midstream")
	scorer := &countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-luna", Reason: "scored"}}
	svc, openAIClient := codexLocalFailureService(t, id, local, scorer, "mid_tier_substitute:\n  model: "+id+"\n")
	rec := httptest.NewRecorder()

	_ = svc.ProxyOpenAIChatCompletion(routerKeyedCtx(), []byte(chatCompletionsTurn), rec, chatCompletionsRequest())

	assert.Equal(t, 1, local.count(), "exactly one upstream request")
	assert.Empty(t, openAIClient.served(), "a committed stream is never re-served")
	out := rec.Body.String()
	assert.Contains(t, out, "partial local answer")
	assert.Contains(t, out, `"error"`, "the client sees the stream fail")
}

// memoryPins is an in-memory session-pin store: just enough for a session to
// remember the model it was served last turn.
type memoryPins struct {
	mu   sync.Mutex
	pins map[string]sessionpin.Pin
}

func pinKey(key [sessionpin.SessionKeyLen]byte, role string) string {
	return string(key[:]) + "/" + role
}

func (m *memoryPins) Get(_ context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pins[pinKey(key, role)]
	return p, ok, nil
}

func (m *memoryPins) Consume(context.Context, [sessionpin.SessionKeyLen]byte, string, router.Strategy) (sessionpin.Pin, bool, error) {
	return sessionpin.Pin{}, false, nil
}

func (m *memoryPins) Upsert(_ context.Context, p sessionpin.Pin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[pinKey(p.SessionKey, p.Role)] = p
	return nil
}

func (m *memoryPins) UpdateUsage(_ context.Context, key [sessionpin.SessionKeyLen]byte, role string, usage sessionpin.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pins[pinKey(key, role)]; ok {
		p.LastServedModel = usage.ServedModel
		m.pins[pinKey(key, role)] = p
	}
	return nil
}

func (*memoryPins) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string, router.Strategy) (int, error) {
	return 0, nil
}

func (*memoryPins) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string, router.Strategy) error {
	return nil
}

func (*memoryPins) IncrementOverloadErrors(context.Context, [sessionpin.SessionKeyLen]byte, string, router.Strategy) (int, error) {
	return 0, nil
}

func (*memoryPins) ResetOverloadErrors(context.Context, [sessionpin.SessionKeyLen]byte, string, router.Strategy) error {
	return nil
}

func (*memoryPins) DisableProvider(context.Context, [sessionpin.SessionKeyLen]byte, string, string, router.Strategy) error {
	return nil
}

func (*memoryPins) ExpireAndDemoteModel(context.Context, sessionpin.Pin, string, sessionpin.DemotionReason) error {
	return nil
}

func (*memoryPins) SweepExpired(context.Context) error { return nil }
