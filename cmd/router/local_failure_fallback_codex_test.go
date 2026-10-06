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
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamingOpenAI is the Codex normal routing target: it streams native
// Responses events, or Chat Completions chunks on that endpoint.
type streamingOpenAI struct {
	mu     sync.Mutex
	models []string
}

func (*streamingOpenAI) IncludedOnlySubscriptions() bool { return true }

func (o *streamingOpenAI) served() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.models...)
}

func (o *streamingOpenAI) Proxy(_ context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	o.mu.Lock()
	o.models = append(o.models, decision.Model)
	o.mu.Unlock()
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
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_n\",\"status\":\"completed\",\"output\":["+done+"]}}\n\n")
	return err
}

func (*streamingOpenAI) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

func codexLocalFailureService(t *testing.T, id string, local *failingLocal, scorer *countingRouter, extraYAML string) (*proxy.Service, *streamingOpenAI) {
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
	svc := proxy.NewService(scorer, providerMap, nil, false, nil, nil, false, providers.ProviderOpenAI, "gpt-5.6-luna", nil).
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
			assert.Contains(t, out, "→ "+tc.served+" · local "+tc.id+" failed")
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
