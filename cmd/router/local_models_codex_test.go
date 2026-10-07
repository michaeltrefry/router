package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/requestcontext"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// recordingOpenAI stands in for the OpenAI client a Codex subscription is
// served on, recording every dispatch it receives.
type recordingOpenAI struct {
	mu     sync.Mutex
	calls  int
	bodies [][]byte
	models []string
}

func (o *recordingOpenAI) record(model string, body []byte) {
	o.mu.Lock()
	o.calls++
	o.bodies = append(o.bodies, body)
	o.models = append(o.models, model)
	o.mu.Unlock()
}

func (o *recordingOpenAI) servedModels() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.models...)
}

func (o *recordingOpenAI) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func (o *recordingOpenAI) Proxy(_ context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	o.record(decision.Model, prep.Body)
	w.Header().Set("Content-Type", "application/json")
	_, err := io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"subscription"}]}]}`)
	return err
}

func (o *recordingOpenAI) Passthrough(_ context.Context, prep providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	o.record("", prep.Body)
	return providers.ErrNotImplemented
}

// codexRouter scores every turn onto the Codex subscription's model.
type codexRouter struct{ calls int }

func (r *codexRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.calls++
	return router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-sol", Reason: "scored"}, nil
}

// codexLocalService boots the composition-root local-model wiring next to an
// OpenAI client; extraYAML extends the model entry or adds top-level keys.
func codexLocalService(t *testing.T, id string, upstream *localUpstream, extraYAML string) (*proxy.Service, *recordingOpenAI, *codexRouter) {
	t.Helper()
	return codexLocalServiceFromEntry(t, id, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"), extraYAML)
}

func codexLocalServiceFromEntry(t *testing.T, id, entryYAML, extraYAML string) (*proxy.Service, *recordingOpenAI, *codexRouter) {
	t.Helper()
	return codexLocalServiceWithPins(t, id, entryYAML, extraYAML, nil)
}

func codexLocalServiceWithPins(t *testing.T, id, entryYAML, extraYAML string, pins sessionpin.Store) (*proxy.Service, *recordingOpenAI, *codexRouter) {
	t.Helper()
	path := writeLocalModelsFile(t, entryYAML+extraYAML)
	openAIClient := &recordingOpenAI{}
	providerMap := map[string]providers.Client{providers.ProviderOpenAI: openAIClient}
	keyed := map[string]struct{}{providers.ProviderOpenAI: {}}
	route, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	rtr := &codexRouter{}
	svc := proxy.NewService(rtr, providerMap, nil, false, nil, pins, false, providers.ProviderOpenAI, "gpt-5.6-luna", nil).
		WithDeploymentKeyedProviders(keyed).
		WithLocalTurnRoute(route.turnRoute)
	return svc, openAIClient, rtr
}

func turnRoutingYAML(id string) string {
	return "turn_routing:\n  model: " + id + "\n"
}

// codexRequest mirrors Codex CLI on a ChatGPT subscription: the OAuth bearer
// pairs with the account header, and the handler stashes the client identity.
func codexRequest(t *testing.T, headers map[string]string) (context.Context, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.Header.Set("User-Agent", "codex_cli_rs/0.147.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.5")
	r.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.codex-subscription.signature")
	r.Header.Set(requestcontext.ChatGPTAccountIDHeader, "acct-test")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	identity := proxy.ClientIdentityFromHeaders(r.Header)
	require.Equal(t, proxy.ClientAppCodex, identity.ClientApp)
	return requestcontext.WithClientIdentity(routerKeyedCtx(), identity), r
}

// responsesItems reassembles the output items a Responses client sees,
// failing on any error or failed-response event.
func responsesItems(t *testing.T, events []clientSSEEvent) []gjson.Result {
	t.Helper()
	var items []gjson.Result
	for _, ev := range events {
		switch ev.name {
		case "error", "response.failed":
			t.Fatalf("stream carried %s: %s", ev.name, ev.data.Raw)
		case "response.output_item.done":
			items = append(items, ev.data.Get("item"))
		}
	}
	return items
}

const codexToolTurn = `{
	"model": "gpt-5.6-sol",
	"stream": true,
	"store": false,
	"instructions": "You are Codex.",
	"reasoning": {"effort": "medium", "summary": "auto"},
	"include": ["reasoning.encrypted_content"],
	"tool_choice": "auto",
	"parallel_tool_calls": false,
	"tools": [{"type": "function", "name": "shell", "description": "Run a command", "strict": false,
		"parameters": {"type": "object", "properties": {"command": {"type": "array", "items": {"type": "string"}}}, "required": ["command"]}}],
	"input": [
		{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "What is in go.mod?"}]}
	]
}`

func TestLocalModel_CodexForcedResponsesTurnStreamsReasoningAndFunctionCall(t *testing.T) {
	const id = "test-local-codex-forced"
	upstream := newScriptedLocalUpstream(t,
		keepAlive+
			chunk(`{"role":"assistant","content":null,"reasoning_content":"Need to "}`, "")+
			keepAlive+
			chunk(`{"reasoning_content":"read the file."}`, "")+
			chunk(`{"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"shell","arguments":""}}]}`, "")+
			keepAlive+
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}`, "")+
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"[\"cat\",\"go.mod\"]}"}}]}`, "")+
			chunk(`{}`, "tool_calls")+
			"data: [DONE]\n\n",
		chunk(`{"role":"assistant","content":"The module is weave-os/router."}`, "")+
			chunk(`{}`, "stop")+
			"data: [DONE]\n\n")
	svc, openAIClient, rtr := codexLocalService(t, id, upstream, "")

	ctx, r := codexRequest(t, map[string]string{proxy.ForceModelHeader: id})
	first := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexToolTurn), first, r))

	events := parseClientSSE(t, first.Body.String())
	require.NotEmpty(t, events, first.Body.String())
	assert.Equal(t, "response.created", events[0].name)
	assert.Equal(t, "response.completed", events[len(events)-1].name, "the stream must end with response.completed: %s", first.Body.String())
	assert.NotContains(t, first.Body.String(), "keep-alive", "upstream comment lines must not leak to the client")

	items := responsesItems(t, events)
	var reasoning, call gjson.Result
	for _, item := range items {
		switch item.Get("type").String() {
		case "reasoning":
			reasoning = item
		case "function_call":
			call = item
		}
	}
	require.True(t, reasoning.Exists(), "the local model's reasoning must surface as a reasoning item: %s", first.Body.String())
	reasoningText := reasoning.Get("summary.#.text").String() + reasoning.Get("content.#.text").String()
	assert.Contains(t, reasoningText, "Need to read the file.")
	require.True(t, call.Exists(), "the tool call must surface as a function_call item: %s", first.Body.String())
	assert.Equal(t, "shell", call.Get("name").String())
	assert.JSONEq(t, `{"command":["cat","go.mod"]}`, call.Get("arguments").String())
	callID := call.Get("call_id").String()
	require.NotEmpty(t, callID)
	// Codex stores the item and serializes its absent content as null.
	replayedReasoning, err := sjson.SetRaw(reasoning.Raw, "content", "null")
	require.NoError(t, err)

	upstream.mu.Lock()
	require.Len(t, upstream.bodies, 1, "the forced local model must be served by its own upstream")
	assert.Equal(t, "/v1/chat/completions", upstream.paths[0])
	assert.Equal(t, "upstream-"+id, gjson.GetBytes(upstream.bodies[0], "model").String())
	assert.Equal(t, "Bearer local-secret", upstream.authz[0], "the configured key authenticates, never the caller's ChatGPT token")
	assert.Equal(t, "shell", gjson.GetBytes(upstream.bodies[0], "tools.0.function.name").String())
	upstream.mu.Unlock()

	// Codex echoes the reasoning and function call and answers it.
	second := `{
		"model": "gpt-5.6-sol",
		"stream": true,
		"store": false,
		"instructions": "You are Codex.",
		"tools": [{"type": "function", "name": "shell", "description": "Run a command", "strict": false,
			"parameters": {"type": "object", "properties": {"command": {"type": "array", "items": {"type": "string"}}}, "required": ["command"]}}],
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "What is in go.mod?"}]},
			` + replayedReasoning + `,
			` + call.Raw + `,
			{"type": "function_call_output", "call_id": ` + jsonString(callID) + `, "output": "module weave-os/router"}
		]
	}`
	ctx, r = codexRequest(t, map[string]string{proxy.ForceModelHeader: id})
	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(second), rec, r))

	upstream.mu.Lock()
	require.Len(t, upstream.bodies, 2)
	sent := upstream.bodies[1]
	upstream.mu.Unlock()
	var assistant, tool gjson.Result
	for _, msg := range gjson.GetBytes(sent, "messages").Array() {
		switch msg.Get("role").String() {
		case "assistant":
			assistant = msg
		case "tool":
			tool = msg
		}
	}
	require.True(t, assistant.Exists(), "the function call must replay as an assistant tool call: %s", sent)
	assert.Equal(t, "shell", assistant.Get("tool_calls.0.function.name").String())
	require.True(t, tool.Exists(), "the function output must replay as a tool message: %s", sent)
	assert.Equal(t, assistant.Get("tool_calls.0.id").String(), tool.Get("tool_call_id").String())
	assert.Contains(t, tool.Get("content").String(), "module weave-os/router")

	var text strings.Builder
	for _, item := range responsesItems(t, parseClientSSE(t, rec.Body.String())) {
		if item.Get("type").String() == "message" {
			text.WriteString(item.Get("content.#.text").String())
		}
	}
	assert.Contains(t, text.String(), "The module is weave-os/router.")
	assert.Zero(t, openAIClient.count(), "a forced local turn never reaches the Codex subscription")
	assert.Zero(t, rtr.calls, "a forced model never consults the scorer")
}

// A reasoning item the router synthesized for a local turn holds nothing
// OpenAI can decrypt, so a later subscription turn must not replay it.
func TestLocalModel_CodexLocalReasoningNeverReachesNativeOpenAI(t *testing.T) {
	const id = "test-local-codex-mixed"
	upstream := newLocalUpstream(t)
	svc, openAIClient, _ := codexLocalService(t, id, upstream, "")

	body := `{
		"model": "gpt-5.6-sol",
		"stream": true,
		"store": false,
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]},
			{"type": "reasoning", "id": "rs_local", "summary": [{"type": "summary_text", "text": "local thought"}], "content": null, "encrypted_content": ` + jsonString(translate.RouterChatReasoningMarker) + `},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "and now?"}]}
		]
	}`
	ctx, r := codexRequest(t, map[string]string{proxy.ForceModelHeader: "gpt-5.6-sol"})
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(body), httptest.NewRecorder(), r))

	openAIClient.mu.Lock()
	defer openAIClient.mu.Unlock()
	require.Len(t, openAIClient.bodies, 1, "the subscription turn is served by OpenAI")
	sent := string(openAIClient.bodies[0])
	assert.NotContains(t, sent, translate.RouterChatReasoningMarker)
	assert.NotContains(t, sent, "local thought")
	assert.Contains(t, sent, "and now?")
}

func TestLocalModel_CodexThinkTagModelStreamsReasoningItem(t *testing.T) {
	const id = "test-local-codex-think"
	upstream := newScriptedLocalUpstream(t,
		chunk(`{"role":"assistant","content":"<think>Check the"}`, "")+
			keepAlive+
			chunk(`{"content":" module.</think>It is weave-os/router."}`, "")+
			chunk(`{}`, "stop")+
			"data: [DONE]\n\n")
	svc, _, _ := codexLocalService(t, id, upstream, "    reasoning_format: think_tags\n")

	ctx, r := codexRequest(t, map[string]string{proxy.ForceModelHeader: id})
	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexToolTurn), rec, r))

	var reasoning, text string
	for _, item := range responsesItems(t, parseClientSSE(t, rec.Body.String())) {
		switch item.Get("type").String() {
		case "reasoning":
			reasoning += item.Get("summary.0.text").String()
		case "message":
			text += item.Get("content.0.text").String()
		}
	}
	assert.Equal(t, "Check the module.", reasoning)
	assert.Contains(t, text, "It is weave-os/router.")
	assert.NotContains(t, rec.Body.String(), "<think>")
}

const codexTitleTurn = `{
	"model": "gpt-5.6-sol",
	"stream": true,
	"tools": [{"type": "function", "name": "shell", "parameters": {"type": "object"}}],
	"text": {"format": {"type": "json_schema", "schema": {
		"type": "object",
		"properties": {"title": {"type": "string"}},
		"required": ["title"],
		"additionalProperties": false
	}}},
	"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Generate a concise task title."}]}]
}`

const codexMainTurn = `{
	"model": "gpt-5.6-sol",
	"stream": true,
	"instructions": "You are Codex.",
	"tools": [{"type": "function", "name": "shell", "parameters": {"type": "object", "properties": {"command": {"type": "array", "items": {"type": "string"}}}}}],
	"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Explore the repository layout."}]}]
}`

func TestLocalModel_CodexTurnRoutingDispatchesTitleAndSubAgentLocally(t *testing.T) {
	const id = "test-local-codex-turns"
	upstream := newLocalUpstream(t)
	svc, openAIClient, _ := codexLocalService(t, id, upstream, turnRoutingYAML(id))

	ctx, r := codexRequest(t, nil)
	title := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexTitleTurn), title, r))

	ctx, r = codexRequest(t, map[string]string{"x-openai-subagent": "collab_spawn"})
	subAgent := httptest.NewRecorder()
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), subAgent, r))

	upstream.mu.Lock()
	require.Len(t, upstream.bodies, 2, "title and spawned sub-agent turns reach the local upstream")
	for _, body := range upstream.bodies {
		assert.Equal(t, "upstream-"+id, gjson.GetBytes(body, "model").String())
	}
	assert.Equal(t, []string{"Bearer local-secret", "Bearer local-secret"}, upstream.authz)
	upstream.mu.Unlock()
	assert.Zero(t, openAIClient.count(), "neither turn is served on the Codex subscription")
	for _, rec := range []*httptest.ResponseRecorder{title, subAgent} {
		events := parseClientSSE(t, rec.Body.String())
		require.NotEmpty(t, events)
		assert.Equal(t, "response.completed", events[len(events)-1].name, rec.Body.String())
	}
}

// Only a spawned sub-agent is local work: Codex's review and approval
// threads, and the main thread, keep the routing they had.
func TestLocalModel_CodexNonSpawnThreadsStayOnSubscription(t *testing.T) {
	const id = "test-local-codex-review"
	upstream := newLocalUpstream(t)
	svc, openAIClient, rtr := codexLocalService(t, id, upstream, turnRoutingYAML(id))

	for _, subagent := range []string{"", "review", "guardian"} {
		ctx, r := codexRequest(t, map[string]string{"x-openai-subagent": subagent})
		require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), httptest.NewRecorder(), r), subagent)
	}

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	assert.Empty(t, upstream.bodies, "no non-spawn thread reaches the local upstream")
	assert.Equal(t, 3, openAIClient.count())
	assert.Equal(t, 3, rtr.calls, "each thread is scored as it was without a local route")
}

// A spawned sub-agent the local model cannot take routes exactly as with no
// local route: under a sub-agent override it is scored on the subscription
// rather than gaining the sub-agent hard pin.
func TestLocalModel_CodexSpawnLocalModelCannotTakeRoutesAsWithoutRoute(t *testing.T) {
	cases := []struct {
		name  string
		entry func(id, baseURL string) string
		extra func(id string) string
	}{
		{
			name:  "no local route",
			entry: func(id, baseURL string) string { return localEntryYAML(id, baseURL, "LOCAL_TEST_KEY") },
			extra: func(string) string { return "" },
		},
		{
			name: "context window too small",
			entry: func(id, baseURL string) string {
				return strings.Replace(localEntryYAML(id, baseURL, "LOCAL_TEST_KEY"), "context_window: 262144", "context_window: 8", 1)
			},
			extra: turnRoutingYAML,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "test-local-codex-spawn-" + string(rune('a'+i))
			upstream := newLocalUpstream(t)
			svc, openAIClient, rtr := codexLocalServiceFromEntry(t, id, tc.entry(id, upstream.baseURL), tc.extra(id))
			svc.WithSubAgentOverride(providers.ProviderOpenAI, "gpt-5.6-luna")

			ctx, r := codexRequest(t, map[string]string{"x-openai-subagent": "collab_spawn"})
			require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), httptest.NewRecorder(), r))

			upstream.mu.Lock()
			assert.Empty(t, upstream.bodies, "the local upstream is not dispatched")
			upstream.mu.Unlock()
			assert.Equal(t, 1, rtr.calls, "the spawned sub-agent is scored")
			assert.Equal(t, []string{"gpt-5.6-sol"}, openAIClient.servedModels(), "the scored subscription model serves the turn")
		})
	}
}
