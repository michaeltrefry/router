package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/dispatch"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/translate"
)

func poolEnrolledCtx(pools ...auth.SubscriptionProvider) context.Context {
	enrolled := make(map[auth.SubscriptionProvider]struct{}, len(pools))
	for _, p := range pools {
		enrolled[p] = struct{}{}
	}
	return context.WithValue(context.Background(), ManagedSubscriptionProvidersContextKey{}, enrolled)
}

func poolBindingService(keyed ...string) *Service {
	keyedSet := map[string]struct{}{}
	for _, p := range keyed {
		keyedSet[p] = struct{}{}
	}
	return &Service{
		clients: dispatch.NewClients(map[string]providers.Client{
			providers.ProviderAnthropic:        nil,
			providers.ProviderAnthropicGateway: nil,
			providers.ProviderOpenAI:           nil,
		}),
		deploymentKeyedProviders: keyedSet,
	}
}

func TestResolveBindingsForDispatch_ForcedModelOnEnrolledPool(t *testing.T) {
	forced := func(model, provider string) router.Decision {
		return router.Decision{Model: model, Provider: provider, Reason: translate.ReasonUserForceModel}
	}

	t.Run("pool-served forced model gets its binding", func(t *testing.T) {
		s := poolBindingService()
		ctx := poolEnrolledCtx(auth.SubscriptionProviderClaude, auth.SubscriptionProviderCodex)

		assert.Equal(t, []catalog.ProviderBinding{{Provider: providers.ProviderAnthropic}},
			s.resolveBindingsForDispatch(ctx, forced("claude-opus-5-5", providers.ProviderAnthropic)))
		assert.Equal(t, []catalog.ProviderBinding{{Provider: providers.ProviderOpenAI}},
			s.resolveBindingsForDispatch(ctx, forced("gpt-6.1-sol", providers.ProviderOpenAI)))
	})

	t.Run("forced model no enrolled pool serves stays unconfigured", func(t *testing.T) {
		s := poolBindingService()

		assert.Empty(t, s.resolveBindingsForDispatch(poolEnrolledCtx(auth.SubscriptionProviderClaude), forced("gpt-6.1-sol", providers.ProviderOpenAI)))
		assert.Empty(t, s.resolveBindingsForDispatch(context.Background(), forced("claude-opus-5-5", providers.ProviderAnthropic)))
	})

	t.Run("pool never joins the keyed set", func(t *testing.T) {
		s := poolBindingService()

		s.resolveBindingsForDispatch(poolEnrolledCtx(auth.SubscriptionProviderClaude), forced("claude-opus-5-5", providers.ProviderAnthropic))

		assert.NotContains(t, s.deploymentKeyedProviders, providers.ProviderAnthropic, "the pool is not a paid fallback credential")
	})

	t.Run("excluded provider stays excluded with a pool", func(t *testing.T) {
		s := poolBindingService()
		ctx := context.WithValue(poolEnrolledCtx(auth.SubscriptionProviderClaude), InstallationExcludedProvidersContextKey{}, []string{providers.ProviderAnthropic})

		assert.Empty(t, s.resolveBindingsForDispatch(ctx, forced("claude-opus-5-5", providers.ProviderAnthropic)))
	})
}

// A forced model pins to the binding the enrolled pool serves rather than a
// keyed gateway that would bill the turn.
func TestForcedModelBinding_PrefersEnrolledPoolOverKeyedGateway(t *testing.T) {
	s := poolBindingService(providers.ProviderAnthropicGateway)

	binding, reason := s.forcedModelBinding(poolEnrolledCtx(auth.SubscriptionProviderClaude), "claude-opus-5-5", providers.ProviderAnthropic)

	assert.Empty(t, reason)
	assert.Equal(t, providers.ProviderAnthropic, binding)

	binding, _ = s.forcedModelBinding(context.Background(), "claude-opus-5-5", providers.ProviderAnthropic)
	assert.Equal(t, providers.ProviderAnthropicGateway, binding, "without a pool the keyed binding serves")
}
