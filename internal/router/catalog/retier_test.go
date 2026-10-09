package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetierModels_AssignsAndRestoresStaticTiers(t *testing.T) {
	t.Cleanup(RestoreTiers)

	require.NoError(t, RetierModels(map[string]Tier{"claude-sonnet-5-5": TierLow, "claude-opus-5-5": TierMid}))

	assert.Equal(t, TierLow, TierFor("claude-sonnet-5-5"))
	assert.Equal(t, TierMid, TierFor("claude-opus-5-5"))
	RestoreTiers()
	assert.Equal(t, TierMid, TierFor("claude-sonnet-5-5"))
	assert.Equal(t, TierHigh, TierFor("claude-opus-5-5"))
}

func TestRetierModels_RejectsUntieredAndUnknownRowsWithoutMutating(t *testing.T) {
	t.Cleanup(RestoreTiers)

	require.Error(t, RetierModels(map[string]Tier{"claude-sonnet-5-5": TierLow, "gpt-5.5": TierMid}), "passthrough-only row")
	require.Error(t, RetierModels(map[string]Tier{"claude-sonnet-5-5": TierLow, "gpt-0": TierMid}), "unknown row")
	require.Error(t, RetierModels(map[string]Tier{"claude-sonnet-5-5": TierUnknown}), "unknown tier")

	assert.Equal(t, TierMid, TierFor("claude-sonnet-5-5"))
}
