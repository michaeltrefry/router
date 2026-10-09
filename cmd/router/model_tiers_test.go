package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/catalog"
)

const userModelTiersYAML = `model_tiers:
  high: [claude-fable-5-1, gpt-6-astra]
  mid: ["claude-opus-*", "gpt-*-sol"]
  default: low
`

func TestParseLocalModels_ModelTiers(t *testing.T) {
	cfg, err := parseLocalModels(strings.NewReader(userModelTiersYAML), envFrom(nil))
	require.NoError(t, err)

	assert.Equal(t, catalog.TierMid, cfg.modelTiers["claude-opus-5-5"], "opus moves from high to mid")
	assert.Equal(t, catalog.TierMid, cfg.modelTiers["gpt-6.1-sol"])
	assert.Equal(t, catalog.TierLow, cfg.modelTiers["claude-sonnet-5-5"], "default tiers every unlisted model")
	assert.Equal(t, catalog.TierLow, cfg.modelTiers["xiaomi/mimo-v2.6-pro"])
	assert.NotContains(t, cfg.modelTiers, "claude-fable-5-1", "an unchanged tier is not an assignment")
	assert.NotContains(t, cfg.modelTiers, "gpt-5.5", "passthrough-only rows stay untiered")
}

func TestParseLocalModels_ModelTiersWithoutDefaultKeepsUnlistedTiers(t *testing.T) {
	cfg, err := parseLocalModels(strings.NewReader("model_tiers:\n  low: [claude-sonnet-5-5]\n"), envFrom(nil))
	require.NoError(t, err)

	assert.Equal(t, map[string]catalog.Tier{"claude-sonnet-5-5": catalog.TierLow}, cfg.modelTiers)
}

func TestParseLocalModels_RejectsInvalidModelTiers(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  error
	}{
		{"pattern matching nothing", "model_tiers:\n  high: [claude-opus-9]\n", errModelTiersNoMatch},
		{"passthrough-only model", "model_tiers:\n  mid: [gpt-5.5]\n", errModelTiersNoMatch},
		{"bad glob", "model_tiers:\n  mid: [\"gpt-[\"]\n", errModelTiersGlob},
		{"model in two tiers", "model_tiers:\n  high: [\"claude-opus-*\"]\n  mid: [claude-opus-5-5]\n", errModelTiersOverlap},
		{"unknown default", "model_tiers:\n  default: medium\n", errLocalModelInvalidField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLocalModels(strings.NewReader(tc.block), envFrom(nil))
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestLoadLocalModels_ModelTiersRetierCatalogAndMappingSources(t *testing.T) {
	path := writeLocalModelsFile(t, userModelTiersYAML+"model_mapping:\n  gpt-5.5: gpt-6.1-sol\n")
	t.Cleanup(func() {
		catalog.RestoreTiers()
		catalog.UntierMappingSources("gpt-5.5")
	})

	_, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path}),
		map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	assert.Equal(t, catalog.TierMid, catalog.TierFor("claude-opus-5-5"))
	assert.Equal(t, catalog.TierLow, catalog.TierFor("claude-sonnet-5-5"))
	assert.Equal(t, catalog.TierMid, catalog.TierFor("gpt-5.5"), "a mapping source takes its target's deployment tier")
}
