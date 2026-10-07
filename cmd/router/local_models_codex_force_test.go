package main

import (
	"net/http/httptest"
	"testing"

	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/sessionpin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	codexForceSession  = "019a0000-0000-7000-8000-0000000000aa"
	codexForceSubAgent = "019a0000-0000-7000-8000-0000000000bb"
	codexForcedModel   = "gpt-5.6-terra"
)

const codexForceCommandTurn = `{
	"model": "gpt-5.6-sol",
	"stream": false,
	"instructions": "You are Codex.",
	"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "/force-model gpt-5.6-terra"}]}]
}`

func codexThreadHeaders(thread string, extra map[string]string) map[string]string {
	h := map[string]string{"session-id": codexForceSession, "thread-id": thread}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// A /force-model in a Codex main thread pins that thread only: a spawned
// sub-agent sharing its session-id keeps the local turn route, unless the
// request itself carries x-weave-force-model.
func TestLocalModel_CodexMainThreadForceLeavesSpawnedSubAgentOnLocalRoute(t *testing.T) {
	const id = "test-local-codex-fthread"
	upstream := newLocalUpstream(t)
	pins := &memoryPins{pins: map[string]sessionpin.Pin{}}
	svc, openAIClient, _ := codexLocalServiceWithPins(t, id, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"), turnRoutingYAML(id), pins)

	serve := func(body string, headers map[string]string) {
		t.Helper()
		ctx, r := codexRequest(t, headers)
		require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(body), httptest.NewRecorder(), r))
	}
	localCalls := func() int {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(upstream.bodies)
	}

	serve(codexForceCommandTurn, codexThreadHeaders(codexForceSession, nil))
	require.Len(t, pins.pins, 1, "the command writes one force pin")

	serve(codexMainTurn, codexThreadHeaders(codexForceSession, nil))
	serve(codexMainTurn, codexThreadHeaders(codexForceSubAgent, map[string]string{"x-openai-subagent": "collab_spawn"}))
	assert.Equal(t, 1, localCalls(), "the spawned sub-agent is served by the local turn route, not the main thread's force")

	serve(codexMainTurn, codexThreadHeaders(codexForceSession, nil))
	serve(codexMainTurn, codexThreadHeaders(codexForceSubAgent, map[string]string{
		"x-openai-subagent": "collab_spawn", proxy.ForceModelHeader: codexForcedModel,
	}))
	assert.Equal(t, 1, localCalls(), "a per-request force header still forces the sub-agent")
	assert.Equal(t, []string{codexForcedModel, codexForcedModel, codexForcedModel}, openAIClient.servedModels(),
		"the main thread stays forced across its turns and the header-forced sub-agent is served on the forced model")
}
