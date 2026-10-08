package catalog

import (
	"testing"

	"weave-os/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTierMappingSources_TiersUntieredSourcesFromTheirTarget(t *testing.T) {
	available := map[string]struct{}{providers.ProviderOpenAI: {}, providers.ProviderAnthropic: {}}
	require.NotContains(t, RoutingTargetSet(available), "gpt-5.5")
	t.Cleanup(func() { UntierMappingSources("gpt-5.5", "claude-fable-5", "gpt-5.4-mini") })

	require.NoError(t, TierMappingSources(map[string]string{
		"gpt-5.5":        "gpt-6.1-sol",
		"claude-fable-5": "claude-fable-5-1",
		"gpt-5.4-mini":   "gpt-6.1-sol",
	}))

	assert.Equal(t, TierHigh, TierFor("gpt-5.5"))
	assert.Equal(t, TierHigh, TierFor("claude-fable-5"))
	assert.Equal(t, TierMid, TierFor("gpt-5.4-mini"), "a tiered source keeps its own tier")
	targets := RoutingTargetSet(available)
	assert.Contains(t, targets, "gpt-5.5")
	assert.Contains(t, targets, "claude-fable-5")
	assert.Contains(t, HMMRoutingTargetSet(available), "gpt-5.5", "HMM and RL candidate universes follow the same tier")

	UntierMappingSources("gpt-5.5", "claude-fable-5", "gpt-5.4-mini")
	assert.Equal(t, TierUnknown, TierFor("gpt-5.5"))
	assert.Equal(t, TierMid, TierFor("gpt-5.4-mini"), "untiering never touches a statically tiered row")
	assert.NotContains(t, RoutingTargetSet(available), "claude-fable-5")
}

func TestTierMappingSources_LeavesSourcesOfUntieredTargetsPassthrough(t *testing.T) {
	t.Cleanup(func() { UntierMappingSources("gpt-5.5") })

	require.NoError(t, TierMappingSources(map[string]string{"gpt-5.5": "gpt-4o"}))

	assert.Equal(t, TierUnknown, TierFor("gpt-5.5"))
}

func TestTierMappingSources_RejectsUnknownModelsWithoutMutating(t *testing.T) {
	t.Cleanup(func() { UntierMappingSources("gpt-5.5") })

	require.Error(t, TierMappingSources(map[string]string{"gpt-5.5": "gpt-6.1-sol", "gpt-0": "gpt-6.1-sol"}))
	require.Error(t, TierMappingSources(map[string]string{"gpt-5.5": "gpt-0"}))

	assert.Equal(t, TierUnknown, TierFor("gpt-5.5"))
}
