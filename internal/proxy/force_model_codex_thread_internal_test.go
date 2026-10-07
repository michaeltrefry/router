package proxy

import (
	"context"
	"net/http"
	"testing"

	"weave-os/router/internal/requestcontext"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	codexTestUA      = "codex_cli_rs/0.147.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.5"
	claudeCodeTestUA = "claude-cli/2.1.0 (external, cli)"
)

func forceKeyForHeaders(t *testing.T, headers map[string]string) [sessionpin.SessionKeyLen]byte {
	t.Helper()
	return forceKeyOnSurface(t, true, headers)
}

func forceKeyOnSurface(t *testing.T, responses bool, headers map[string]string) [sessionpin.SessionKeyLen]byte {
	t.Helper()
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	ctx := requestcontext.WithClientIdentity(context.Background(), ClientIdentityFromHeaders(h))
	if responses {
		ctx = context.WithValue(ctx, responsesSurfaceContextKey{}, true)
	}
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"task"}]}`))
	require.NoError(t, err)
	threadKey := deriveSessionKeyForRequest(ctx, env, "api-key")
	return deriveForceModelSessionKeyForRequest(ctx, env, "api-key", threadKey)
}

func TestForceModelSessionKey_CodexSpawnedSubAgentIsThreadScoped(t *testing.T) {
	const session = "019a0000-0000-7000-8000-000000000001"
	sessionOnly := forceKeyForHeaders(t, map[string]string{"User-Agent": codexTestUA, "Session-Id": session})
	main := forceKeyForHeaders(t, map[string]string{"User-Agent": codexTestUA, "Session-Id": session, "Thread-Id": session})
	spawn := func(thread string) [sessionpin.SessionKeyLen]byte {
		return forceKeyForHeaders(t, map[string]string{
			"User-Agent": codexTestUA, "Session-Id": session, "Thread-Id": thread, "X-Openai-Subagent": "collab_spawn",
		})
	}

	assert.Equal(t, sessionOnly, main, "a main thread keeps the session-scoped force key")
	assert.NotEqual(t, main, spawn("019a0000-0000-7000-8000-000000000002"), "a spawned sub-agent does not read the main thread's force")
	assert.Equal(t, spawn("019a0000-0000-7000-8000-000000000002"), spawn("019a0000-0000-7000-8000-000000000002"), "a sub-agent's force key is stable across its turns")
	assert.NotEqual(t, spawn("019a0000-0000-7000-8000-000000000002"), spawn("019a0000-0000-7000-8000-000000000003"), "sibling sub-agents are separate")

	for _, kind := range []string{"review", "compact", "guardian"} {
		other := forceKeyForHeaders(t, map[string]string{
			"User-Agent": codexTestUA, "Session-Id": session, "Thread-Id": "019a0000-0000-7000-8000-000000000004", "X-Openai-Subagent": kind,
		})
		assert.Equal(t, main, other, "%s threads keep the session force", kind)
	}
}

func TestForceModelSessionKey_CodexSpawnedSubAgentOffResponsesKeepsSessionKey(t *testing.T) {
	const session = "019a0000-0000-7000-8000-000000000001"
	main := forceKeyOnSurface(t, false, map[string]string{"User-Agent": codexTestUA, "Session-Id": session, "Thread-Id": session})
	spawn := forceKeyOnSurface(t, false, map[string]string{
		"User-Agent": codexTestUA, "Session-Id": session, "Thread-Id": "019a0000-0000-7000-8000-000000000002", "X-Openai-Subagent": "collab_spawn",
	})
	assert.Equal(t, main, spawn, "a collab_spawn request outside Responses ingress keeps the session force key")
}

func TestForceModelSessionKey_ClaudeCodeUnchangedBySubAgentHeaders(t *testing.T) {
	const session = "cc-session"
	plain := forceKeyForHeaders(t, map[string]string{"User-Agent": claudeCodeTestUA, requestcontext.ClaudeCodeSessionHeader: session})
	withCodexHeaders := forceKeyForHeaders(t, map[string]string{
		"User-Agent": claudeCodeTestUA, requestcontext.ClaudeCodeSessionHeader: session,
		"Thread-Id": "other-thread", "X-Openai-Subagent": "collab_spawn",
	})
	assert.Equal(t, plain, withCodexHeaders)
}
