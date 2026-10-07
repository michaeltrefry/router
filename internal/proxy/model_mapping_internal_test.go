package proxy

import (
	"context"
	"testing"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/turntype"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mapped decision drops the scorer's arm selection, which names an
// upstream of the original model, without touching the original's metadata.
func TestMapModel_DropsArmSelectionOnACopy(t *testing.T) {
	svc := NewService(nil, map[string]providers.Client{providers.ProviderAnthropic: &flowSpanProvider{}},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithModelMapping(ModelMapping{"claude-opus-5": "claude-opus-5-5"})
	original := &router.RoutingMetadata{
		RouteID:             "route-1",
		SelectedArmID:       "arm-opus-5",
		SelectedRosterArmID: "roster-opus-5",
		SelectedUpstreamID:  "upstream-opus-5",
		BindingIndex:        2,
	}
	res := turnLoopResult{
		TurnType: turntype.MainLoop,
		Decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5", Reason: "cluster", Metadata: original},
	}

	svc.mapModel(context.Background(), &res)

	require.Equal(t, "claude-opus-5-5", res.Decision.Model)
	require.NotNil(t, res.Decision.Metadata)
	assert.Empty(t, res.Decision.Metadata.SelectedArmID)
	assert.Empty(t, res.Decision.Metadata.SelectedRosterArmID)
	assert.Empty(t, res.Decision.Metadata.SelectedUpstreamID)
	assert.Zero(t, res.Decision.Metadata.BindingIndex)
	assert.Equal(t, "route-1", res.Decision.Metadata.RouteID, "routing context other than the arm is kept")
	assert.Equal(t, "arm-opus-5", original.SelectedArmID)
	assert.Equal(t, "roster-opus-5", original.SelectedRosterArmID)
	assert.Equal(t, "upstream-opus-5", original.SelectedUpstreamID)
	assert.Equal(t, 2, original.BindingIndex)
	assert.Same(t, original, res.SubstitutedFrom.Metadata, "the router's own pick keeps its metadata")
}
