package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/translate"
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

	assert.Contains(t, got, testOpus)
	assert.Contains(t, got, "claude-haiku-4-5")
	assert.Contains(t, got, "claude-opus-4-8", "an untiered catalog model is in no class")
	assert.NotContains(t, got, "claude-sonnet-5-5")
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

func TestModelClassUnavailable_KeepsAllowlistError(t *testing.T) {
	err := fmt.Errorf("allowlist: %w", cluster.ErrAllowlistEmptiesPool)
	assert.Same(t, err, modelClassUnavailable(ctxWithModelClass(catalog.TierLow), err))
}

// A utility turn whose hard-pin resolver finds nothing in the class reports
// the class, not a cluster outage.
func TestTurnLoop_HardPinTurnWithEmptyClassFailsAsClassUnavailable(t *testing.T) {
	var seen HardPinRequest
	svc := NewService(&tierProbeRouter{}, nil, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic)).
		WithHardPinResolver(func(req HardPinRequest) (string, string, bool) {
			seen = req
			return "", "", false
		})
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-haiku-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"properties":{"title":{"type":"string"}}}}}}`))
	require.NoError(t, err)
	feats := env.RoutingFeatures(false)
	ctx := ctxWithModelClass(catalog.TierHigh)

	_, err = svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model, ExcludedModels: svc.excludedModelsForRequest(ctx)})

	var unavailable *ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Contains(t, seen.ExcludedModels, "claude-haiku-4-5", "the resolver is offered only the class")
}

func TestTurnLoop_SavedForceOutsideClassRejectsInsideServes(t *testing.T) {
	newSvc := func() (*Service, *tierProbeRouter) {
		fr := &tierProbeRouter{available: map[string]struct{}{"claude-haiku-4-5": {}}}
		store := &overwritingPinStore{pin: sessionpin.Pin{
			Provider:    providers.ProviderAnthropic,
			Model:       "claude-opus-5",
			Reason:      translate.ReasonUserForceModel,
			PinnedUntil: pinNeverExpires,
		}, found: true}
		return NewService(fr, nil, nil, false, nil, store, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic)), fr
	}
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)

	svc, fr := newSvc()
	_, err := svc.runTurnLoop(ctxWithModelClass(catalog.TierLow), env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model})
	require.ErrorIs(t, err, ErrForcedModelExcluded)
	cls, _ := ClassifyDispatchError(err)
	assert.Equal(t, http.StatusBadRequest, cls.Status)
	assert.Empty(t, fr.captured, "the request fails instead of routing around the force")

	svc, _ = newSvc()
	res, err := svc.runTurnLoop(ctxWithModelClass(catalog.TierHigh), env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-5", res.Decision.Model)
}

func TestTurnLoop_StickyPinOutsideModelClassReroutes(t *testing.T) {
	store := &overwritingPinStore{pin: sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       testOpus,
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(time.Hour),
	}, found: true}
	fr := &tierProbeRouter{available: map[string]struct{}{testOpus: {}, "claude-haiku-4-5": {}}}
	svc := NewService(fr, nil, nil, false, nil, store, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
	env := forceCommandEnv(t)
	feats := env.RoutingFeatures(false)
	ctx := ctxWithModelClass(catalog.TierLow)

	res, err := svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model, ExcludedModels: svc.excludedModelsForRequest(ctx)})

	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", res.Decision.Model)
	assert.False(t, res.StickyHit)
}

func TestRescueDecisions_SkipCandidatesOutsideModelClass(t *testing.T) {
	svc := NewService(nil, nil, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
	failed := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-5-5"}
	candidates := []string{"claude-opus-5-5", "claude-sonnet-4-6"}

	got := svc.rescueDecisions(ctxWithModelClass(catalog.TierMid), failed, candidates, "sibling", 0, 0, 0)

	models := make([]string, 0, len(got))
	for _, d := range got {
		models = append(models, d.Model)
	}
	assert.Equal(t, []string{"claude-sonnet-4-6"}, models)
}

func TestSubscriptionCoveredTarget_RefusesRequestedModelOutsideClass(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-test"}}
	req := router.Request{RequestedModel: "claude-opus-5"}

	_, _, covered := subscriptionCoveredTarget(context.Background(), headers, req)
	require.True(t, covered, "precondition: the subscription covers the requested model")
	_, _, covered = subscriptionCoveredTarget(ctxWithModelClass(catalog.TierLow), headers, req)
	assert.False(t, covered)
}

func TestRequestNarrowsModels(t *testing.T) {
	assert.False(t, requestNarrowsModels(context.Background()))
	assert.True(t, requestNarrowsModels(ctxWithModelClass(catalog.TierLow)), "a class request bypasses the semantic cache")
}

// legacyForcePinStore holds a thread-scoped force pin only, as written before
// forces moved to their own session role.
type legacyForcePinStore struct{ overwritingPinStore }

func (s *legacyForcePinStore) Get(ctx context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	if role == forceModelSessionRole {
		return sessionpin.Pin{}, false, nil
	}
	return s.overwritingPinStore.Get(ctx, key, role)
}

func TestTurnLoop_LegacyForceOutsideClassRejects(t *testing.T) {
	store := &legacyForcePinStore{overwritingPinStore{pin: sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-opus-5",
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: pinNeverExpires,
	}, found: true}}
	fr := &tierProbeRouter{available: map[string]struct{}{"claude-haiku-4-5": {}}}
	svc := NewService(fr, nil, nil, false, nil, store, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed(providers.ProviderAnthropic))
	// A tool-bearing main-loop turn reads the legacy pin from its pin role.
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-opus-4-7","max_tokens":256,` +
		`"tools":[{"name":"noop","description":"placeholder","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"fix the build"}]}`))
	require.NoError(t, err)
	feats := env.RoutingFeatures(false)
	ctx := ctxWithModelClass(catalog.TierLow)

	_, err = svc.runTurnLoop(ctx, env, feats, "key-1", uuid.New(), "", nil,
		router.Request{RequestedModel: feats.Model, ExcludedModels: svc.excludedModelsForRequest(ctx)})

	require.ErrorIs(t, err, ErrForcedModelExcluded)
	assert.Empty(t, fr.captured, "the request fails instead of routing around the force")
}

func TestCallerModelPassthrough_OutsideClassReportsClass(t *testing.T) {
	svc := NewService(nil, nil, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	_, err := svc.callerModelPassthroughDecision(ctxWithModelClass(catalog.TierLow), router.Request{RequestedModel: "claude-opus-5"})

	var unavailable *ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
}
