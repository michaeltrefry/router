package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
)

func ctxWithModelClass(class catalog.Tier) context.Context {
	return context.WithValue(context.Background(), ModelClassContextKey{}, class)
}

// emptyPoolRouter is tierProbeRouter reporting an emptied pool the way the
// cluster scorer does.
type emptyPoolRouter struct{ tierProbeRouter }

func (r *emptyPoolRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	d, err := r.tierProbeRouter.Route(ctx, req)
	if err != nil {
		return d, fmt.Errorf("every candidate excluded: %w", cluster.ErrNoEligibleProvider)
	}
	return d, nil
}

func TestExcludedModelsForRequest_ModelClassExcludesEveryOtherTier(t *testing.T) {
	s := &Service{availableModels: map[string]struct{}{testOpus: {}, "claude-sonnet-5-5": {}, "claude-haiku-4-5": {}}}

	got := s.excludedModelsForRequest(ctxWithModelClass(catalog.TierMid))

	assert.Equal(t, map[string]struct{}{testOpus: {}, "claude-haiku-4-5": {}}, got)
	assert.Empty(t, s.excludedModelsForRequest(context.Background()), "no class, no exclusion")
}

func TestTurnLoop_ModelClassRoutesOnlyWithinClass(t *testing.T) {
	available := map[string]struct{}{"claude-haiku-4-5": {}, "claude-opus-5-5": {}}
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)
	route := func(ctx context.Context) router.Decision {
		svc := NewService(&tierProbeRouter{available: available}, nil, nil, false, nil, nil, false,
			providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
		res, err := svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
			router.Request{RequestedModel: feats.Model, ExcludedModels: svc.excludedModelsForRequest(ctx)})
		require.NoError(t, err)
		return res.Decision
	}

	assert.Equal(t, "claude-haiku-4-5", route(context.Background()).Model, "the probe router prefers the lowest tier")
	assert.Equal(t, "claude-opus-5-5", route(ctxWithModelClass(catalog.TierHigh)).Model)
}

func TestTurnLoop_EmptyModelClassFailsAsClassUnavailable(t *testing.T) {
	svc := NewService(&emptyPoolRouter{tierProbeRouter{available: map[string]struct{}{"claude-haiku-4-5": {}}}}, nil, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)
	ctx := ctxWithModelClass(catalog.TierHigh)

	_, err := svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model, ExcludedModels: svc.excludedModelsForRequest(ctx)})

	var unavailable *ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Equal(t, catalog.TierHigh, unavailable.Class)
	cls, ok := ClassifyDispatchError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, cls.Status)
	assert.Contains(t, cls.Message, ModelClassUnavailableCode)
	assert.Equal(t, ModelClassUnavailableCode, OpenAIErrorCode(cls.Kind))
}

func TestModelClassUnavailable_LeavesErrorsWithoutClassAlone(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", cluster.ErrNoEligibleProvider)
	assert.Same(t, err, modelClassUnavailable(context.Background(), err))
	other := fmt.Errorf("upstream 500")
	assert.Same(t, other, modelClassUnavailable(ctxWithModelClass(catalog.TierLow), other))
}

func TestSetModelClassHeader(t *testing.T) {
	h := http.Header{}
	setModelClassHeader(h, "claude-haiku-4-5")
	assert.Equal(t, "low", h.Get(HeaderRouterModelClass))
	setModelClassHeader(h, "gpt-5.5")
	assert.Empty(t, h.Get(HeaderRouterModelClass), "an untiered model clears a stale class")
}

func TestDispatchPlanned_SetsServedModelClassHeader(t *testing.T) {
	primary := &fakeClient{name: providers.ProviderFireworks, outcomes: []fakeOutcome{{writeBytes: []byte("ok")}}}
	s := newServiceWithProviders(t, map[string]providers.Client{providers.ProviderFireworks: primary})
	rec := httptest.NewRecorder()
	buf := newPreludeBuffer(rec)
	in := plannedInputs(rec, buf, []catalog.ProviderBinding{{Provider: providers.ProviderFireworks}}, nil)
	in.initialDecision.Model = "claude-haiku-4-5"

	_, err := s.dispatchWithFallback(context.Background(), in)

	require.NoError(t, err)
	assert.Equal(t, "low", rec.Header().Get(HeaderRouterModelClass))
}
