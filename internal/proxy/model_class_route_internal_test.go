package proxy

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/turntype"
)

func lowMembersCtx(members ...string) context.Context {
	set := map[string]struct{}{}
	for _, m := range members {
		set[m] = struct{}{}
	}
	return context.WithValue(ctxWithModelClass(catalog.TierLow), ModelClassMembersContextKey{}, set)
}

var anthropicOnly = map[string]struct{}{providers.ProviderAnthropic: {}}

// The /v1/route dry run reports what a low-class turn would be served on: the
// first servable entry of the strict order, not the scorer's pick.
func TestRoute_LowClassReportsTheFirstServableOrderEntry(t *testing.T) {
	order := []string{"claude-haiku-4-5", "claude-sonnet-4-5"}
	svc := NewService(&tierProbeRouter{available: map[string]struct{}{"claude-sonnet-4-5": {}}}, nil, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).WithModelClassOrder(ModelClassOrder{catalog.TierLow: order})
	ctx := lowMembersCtx(order...)

	d, err := svc.Route(ctx, router.Request{EnabledProviders: anthropicOnly})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", d.Model)

	d, err = svc.Route(ctx, router.Request{EnabledProviders: anthropicOnly, ExcludedModels: map[string]struct{}{"claude-haiku-4-5": {}}})
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-5", d.Model)
}

// A high-class dry run whose routing finds nothing reports the order's entry.
func TestRoute_EmptyHighClassReportsTheOrderEntry(t *testing.T) {
	svc := NewService(&emptyPoolRouter{}, nil, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithModelClassOrder(ModelClassOrder{catalog.TierHigh: {"claude-opus-5-5"}})

	d, err := svc.Route(ctxWithModelClass(catalog.TierHigh), router.Request{EnabledProviders: anthropicOnly})

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-5-5", d.Model)
}

// A request the class already refused, such as a caller-model passthrough
// outside it, is not rescued onto a router-chosen order entry.
func TestRescueEmptyClass_LeavesAClassRefusalAlone(t *testing.T) {
	svc := (&Service{}).WithModelClassOrder(ModelClassOrder{catalog.TierHigh: {"claude-opus-5-5"}})
	ctx := ctxWithModelClass(catalog.TierHigh)
	res := turnLoopResult{TurnType: turntype.MainLoop}
	req := router.Request{EnabledProviders: anthropicOnly}

	refusal := &ModelClassUnavailableError{Class: catalog.TierHigh, Err: cluster.ErrNoEligibleProvider}
	assert.False(t, svc.rescueEmptyClass(ctx, &res, req, refusal))
	assert.True(t, svc.rescueEmptyClass(ctx, &res, req, fmt.Errorf("empty: %w", cluster.ErrNoEligibleProvider)), "precondition: an emptied pool is rescued")
}

// An order entry the request's safety filters exclude never serves.
func TestClassOrderTarget_HonorsSafetyExclusions(t *testing.T) {
	req := router.Request{EnabledProviders: anthropicOnly, SafetyExcludedModels: map[string]struct{}{"claude-haiku-4-5": {}}}
	_, ok := classOrderTarget(router.Decision{}, "claude-haiku-4-5", turntype.MainLoop, req)
	assert.False(t, ok)
	req = router.Request{EnabledProviders: anthropicOnly, UnsignedHistoryExcludedModels: map[string]struct{}{"claude-haiku-4-5": {}}}
	_, ok = classOrderTarget(router.Decision{}, "claude-haiku-4-5", turntype.MainLoop, req)
	assert.False(t, ok)
}

func TestClassOrderKeyDomain_IsPerClass(t *testing.T) {
	assert.NotEqual(t, classOrderKeyDomain(catalog.TierHigh), classOrderKeyDomain(catalog.TierLow))
	assert.NotEqual(t, classOrderKeyDomain(catalog.TierMid), classOrderKeyDomain(catalog.TierHigh))
}
