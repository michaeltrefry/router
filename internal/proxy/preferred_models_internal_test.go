package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A turn rerouted around its local model routes as if no rule existed, so a
// preferred local model must not rank the rule's matches there.
func TestExpandPreferredModels_SkipsRulesWhenLocalRoutingDisabled(t *testing.T) {
	s := &Service{
		availableModels:   map[string]struct{}{"claude-opus-5": {}, "claude-opus-5-5": {}, "gpt-5.5": {}},
		modelMapping:      ModelMapping{"claude-opus-5": "claude-opus-5-5"},
		substitutionRules: []SubstitutionRule{{Match: "gpt-*", Provider: "local_x", Model: "x"}},
	}

	assert.Equal(t, []string{"x", "gpt-5.5", "claude-opus-5-5", "claude-opus-5"},
		s.expandPreferredModels(context.Background(), []string{"x", "claude-opus-5-5"}))
	assert.Equal(t, []string{"x", "claude-opus-5-5", "claude-opus-5"},
		s.expandPreferredModels(withLocalRoutingDisabled(context.Background()), []string{"x", "claude-opus-5-5"}))
}
