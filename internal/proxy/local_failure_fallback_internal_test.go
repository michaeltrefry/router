package proxy

import (
	"context"
	"testing"

	"weave-os/router/internal/observability/otel"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

func TestLocalFailureMarker_FollowsNormalRouteBadgeRules(t *testing.T) {
	fb := &localFailureFallback{local: router.Decision{Provider: providers.LocalProviderName("box"), Model: "box"}}
	normal := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}

	hardPinned := turnLoopResult{Decision: normal, HardPinned: true}
	assert.Empty(t, fb.marker(hardPinned), "a hard pin's badge stays hidden")

	dropped := turnLoopResult{Decision: normal, HardPinned: true, ForcedPinDropped: true, ForcedPinModel: "box"}
	assert.Equal(t,
		routingMarkerPrefix+"claude-haiku-4-5 · "+markerReasonForcedPinDropped+" (box) · box (local) failed\n\n",
		fb.marker(dropped), "a dropped force pin is reported even on a hard pin")

	sameModel := turnLoopResult{Decision: normal, PriorServedModel: "claude-haiku-4-5"}
	assert.Empty(t, fb.marker(sameModel), "the model the session already sees carries no badge")
}

// A reroute after a failed local model runs the turn loop a second time; the
// request's Responses transforms are recorded once.
func TestRunTurnLoop_RerouteDoesNotReRecordTransforms(t *testing.T) {
	coll := newLogCollector(t)
	_, em := newServiceWithEmitter(t, CaptureOff, nil, coll.server.URL)
	svc := NewService(&tierProbeRouter{available: map[string]struct{}{"claude-haiku-4-5": {}}}, nil, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)
	buf := otel.NewBuffer(em)
	ctx := context.WithValue(buf.WithContext(context.Background()), responsesTransformsContextKey{},
		[]translate.ResponseTransform{{Code: "test_transform", Action: "dropped", Path: "input[0]"}})

	for _, turnCtx := range []context.Context{ctx, withLocalRoutingDisabled(ctx)} {
		_, err := svc.runTurnLoop(turnCtx, env, feats, "key-1", uuid.New(), "", nil, router.Request{RequestedModel: feats.Model})
		require.NoError(t, err)
	}
	otel.Flush(ctx)
	require.NoError(t, em.Shutdown(context.Background()))

	coll.mu.Lock()
	defer coll.mu.Unlock()
	transforms := 0
	for _, b := range coll.bodies {
		var req collogspb.ExportLogsServiceRequest
		require.NoError(t, proto.Unmarshal(b, &req))
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					for _, kv := range lr.Attributes {
						if kv.Key == "translation.code" && kv.GetValue().GetStringValue() == "test_transform" {
							transforms++
						}
					}
				}
			}
		}
	}
	assert.Equal(t, 1, transforms)
}
