package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/proxy"
)

func TestParseLocalModels_ModelMapping(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := "models:\n" + localEntryYAML("m1", "http://localhost:1/v1", "KEY_A")
	parse := func(t *testing.T, block string) (localModelsConfig, error) {
		t.Helper()
		return parseLocalModels(strings.NewReader(entries+block), env)
	}

	t.Run("omitted block maps nothing", func(t *testing.T) {
		cfg, err := parse(t, "")
		require.NoError(t, err)
		assert.Empty(t, cfg.modelMapping)
	})
	t.Run("catalog targets load", func(t *testing.T) {
		cfg, err := parse(t, "model_mapping:\n  claude-opus-5: claude-opus-5-5\n  gpt-5.5: gpt-6.1-sol\n")
		require.NoError(t, err)
		assert.Equal(t, proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5", "gpt-5.5": "gpt-6.1-sol"}, cfg.modelMapping)
	})
	t.Run("mapping alone needs no local models", func(t *testing.T) {
		cfg, err := parseLocalModels(strings.NewReader("model_mapping:\n  claude-opus-5: claude-opus-5-5\n"), env)
		require.NoError(t, err)
		assert.Equal(t, proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5"}, cfg.modelMapping)
	})
	rejected := []struct {
		name  string
		block string
		want  error
	}{
		{"target outside the catalog", "model_mapping:\n  claude-opus-5: claude-opus-9\n", errModelMappingTarget},
		{"local model target", "model_mapping:\n  claude-opus-5: m1\n", errModelMappingTarget},
		{"empty target", "model_mapping:\n  claude-opus-5: \"\"\n", errModelMappingTarget},
		{"source outside the catalog", "model_mapping:\n  claude-opus-9: claude-opus-5-5\n", errModelMappingSource},
		{"self mapping", "model_mapping:\n  claude-opus-5: claude-opus-5\n", errModelMappingChain},
		{"chained mapping", "model_mapping:\n  claude-opus-5: claude-opus-5-5\n  claude-opus-5-5: claude-fable-5-1\n", errModelMappingChain},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := parse(t, tc.block)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
