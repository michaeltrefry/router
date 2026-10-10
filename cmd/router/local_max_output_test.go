package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
)

// bigOutputBody is a Claude Code-style turn asking for 64K output tokens.
const bigOutputBody = `{"model":"claude-sonnet-4-6","max_tokens":64000,"stream":true,"system":"You are Claude Code.",` +
	`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
	`"messages":[{"role":"user","content":"write the whole module"}]}`

// upstreamOutputBudget is the output-token budget a local upstream received.
func upstreamOutputBudget(t *testing.T, u *localUpstream) int64 {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.Len(t, u.bodies, 1)
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if v := gjson.GetBytes(u.bodies[0], key); v.Exists() {
			return v.Int()
		}
	}
	t.Fatalf("no output budget in %s", u.bodies[0])
	return 0
}

// A local model's output budget is its own (half its context window by
// default), not the 8,192 fallback for models the router does not know.
func TestLocalModel_OutputBudgetIsNotCappedAt8192(t *testing.T) {
	first, second := newLocalUpstream(t), newLocalUpstream(t)
	svc, _ := lowTierService(t, "test-mot-a1", "test-mot-a2", first.baseURL, second.baseURL)

	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(bigOutputBody), httptest.NewRecorder(), claudeCodeRequest("")))

	assert.Equal(t, int64(64000), upstreamOutputBudget(t, first), "context_window 262144 → default cap 131072, so 64000 passes")
}

func TestLocalModel_ExplicitMaxOutputTokensClamps(t *testing.T) {
	first, second := newLocalUpstream(t), newLocalUpstream(t)
	svc := lowTierServiceWithFirstEntry(t, "test-mot-b1", "test-mot-b2", first.baseURL, second.baseURL, "    max_output_tokens: 16384\n",
		&streamingAnthropic{}, router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"})

	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(bigOutputBody), httptest.NewRecorder(), claudeCodeRequest("")))

	assert.Equal(t, int64(16384), upstreamOutputBudget(t, first))
}

func TestParseLocalModels_RejectsInvalidMaxOutputTokens(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	for name, field := range map[string]string{
		"negative":         "    max_output_tokens: -1\n",
		"above the window": "    max_output_tokens: 300000\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseLocalModels(strings.NewReader("models:\n"+localEntryYAML("m1", "http://localhost:1/v1", "KEY_A")+field), env)
			require.ErrorIs(t, err, errLocalModelMaxOutput)
		})
	}
}
