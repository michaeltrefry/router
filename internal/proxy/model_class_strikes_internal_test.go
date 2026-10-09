package proxy

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"
)

func decisionsFor(models ...string) []router.Decision {
	out := make([]router.Decision, 0, len(models))
	for _, m := range models {
		out = append(out, router.Decision{Model: m})
	}
	return out
}

func modelsOf(ds []router.Decision) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Model)
	}
	return out
}

func TestWithSessionState(t *testing.T) {
	list := decisionsFor("a", "b", "c")

	assert.Equal(t, []string{"a", "c"}, modelsOf(withSessionState(list, map[string]struct{}{"b": {}}, "")), "a struck entry is skipped, order kept")
	assert.Equal(t, []string{"a", "b", "c"}, modelsOf(withSessionState(list, map[string]struct{}{"a": {}, "b": {}, "c": {}}, "")), "strikes are soft")
	assert.Equal(t, []string{"c", "a", "b"}, modelsOf(withSessionState(list, nil, "c")), "a refusal re-pin goes first")
	assert.Equal(t, []string{"a", "b", "c"}, modelsOf(withSessionState(list, nil, "z")), "a re-pin outside the list changes nothing")
	assert.Equal(t, []string{"a", "b", "c"}, modelsOf(list), "the input is not mutated")
}

// A class-ordered turn skips a model the session struck on an earlier turn,
// and records its own state under the same key.
func TestTurnLoop_ClassOrderSkipsSessionStrikes(t *testing.T) {
	ctx := lowMembersCtx("claude-haiku-4-5", "gpt-4.1-mini")
	store := &overwritingPinStore{pin: sessionpin.Pin{
		Strategy:      router.StrategyFromContext(ctx),
		Provider:      providers.ProviderAnthropic,
		Model:         "claude-haiku-4-5",
		Reason:        reasonModelClassOrder,
		PinnedUntil:   time.Now().Add(-time.Minute),
		DemotedModels: []string{"claude-haiku-4-5"},
	}, found: true}
	svc := NewService(&tierProbeRouter{}, nil, nil, false, nil, store, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic, providers.ProviderOpenAI)).
		WithModelClassOrder(ModelClassOrder{catalog.TierLow: {"claude-haiku-4-5", "gpt-4.1-mini"}})
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)

	res, err := svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model, EnabledProviders: keyed(providers.ProviderAnthropic, providers.ProviderOpenAI), ExcludedModels: svc.excludedModelsForRequest(ctx)})

	require.NoError(t, err)
	assert.Equal(t, "gpt-4.1-mini", res.Decision.Model)
	assert.NotEqual(t, [sessionpin.SessionKeyLen]byte{}, res.SessionKey, "the turn's own strikes are recorded under the order's key")
}
