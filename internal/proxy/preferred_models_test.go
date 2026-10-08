package proxy_test

import (
	"context"
	"testing"

	"weave-os/router/internal/proxy"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func preferringCtx(preferred ...string) context.Context {
	return context.WithValue(authedCtx(uuid.New().String()), proxy.InstallationPreferredModelsContextKey{}, preferred)
}

// The scorer only ranks the models it selects, so preferring a mapping target
// ranks the scorer model mapped onto it, and the turn serves the target.
func TestPreferredModels_MappingTargetRanksItsSource(t *testing.T) {
	f := newRuleFixture(t, "test-pref-map", opus5Decision, defaultTestMapping)

	f.serve(t, preferringCtx("claude-opus-5-5"), pinTestBody, nil)

	require.NotNil(t, f.scorer.capturedReq)
	assert.Equal(t, []string{"claude-opus-5-5", "claude-opus-5"}, f.scorer.capturedReq.PreferredModels)
	require.Len(t, f.anthropic.proxyBodies, 1)
	assert.Equal(t, "claude-opus-5-5", upstreamModel(t, f.anthropic.proxyBodies[0]))
}

// A preferred local model ranks every routable model a substitution rule
// serves on it, matched after mapping.
func TestPreferredModels_LocalModelRanksRuleMatches(t *testing.T) {
	f := newRuleFixture(t, "test-pref-local", opus5Decision, defaultTestMapping, "claude-sonnet-5-5")

	f.serve(t, preferringCtx(f.model, "gpt-5.5"), pinTestBody, nil)

	require.NotNil(t, f.scorer.capturedReq)
	assert.Equal(t, []string{f.model, "claude-sonnet-5", "claude-sonnet-5-5", "gpt-5.5"}, f.scorer.capturedReq.PreferredModels)
}

func TestPreferredModels_UnchangedWithoutMappingOrRules(t *testing.T) {
	f := newRuleFixture(t, "test-pref-plain", opus5Decision, nil)

	f.serve(t, preferringCtx("claude-opus-5-5", "gpt-5.5"), pinTestBody, nil)

	require.NotNil(t, f.scorer.capturedReq)
	assert.Equal(t, []string{"claude-opus-5-5", "gpt-5.5"}, f.scorer.capturedReq.PreferredModels)
}
