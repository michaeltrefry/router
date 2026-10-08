package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/requestcontext"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/subscriptions"
)

// poolLeaser hands out one account per enrolled pool, or none when drained.
type poolLeaser struct {
	mu      sync.Mutex
	drained bool
	leased  []subscriptions.Provider
}

func (p *poolLeaser) Lease(_ context.Context, _ auth.SubscriptionOwner, provider subscriptions.Provider, _ string) (subscriptions.Lease, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leased = append(p.leased, provider)
	if p.drained {
		return subscriptions.Lease{}, true, subscriptions.ErrNoAvailableAccount
	}
	lease := subscriptions.Lease{AccountID: "account-" + string(provider), AccessToken: "pool-token-" + string(provider), State: auth.SubscriptionAccountStateActive, Tier: auth.SubscriptionTierPersonal}
	if provider == subscriptions.ProviderCodex {
		lease.ProviderAccount = "workspace-pool"
	}
	return lease, true, nil
}

func (p *poolLeaser) Cooldown(context.Context, auth.SubscriptionOwner, subscriptions.Provider, string, time.Time) error {
	return nil
}

func (p *poolLeaser) Disable(context.Context, auth.SubscriptionOwner, subscriptions.Provider, string) error {
	return nil
}

func (p *poolLeaser) leases() []subscriptions.Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]subscriptions.Provider(nil), p.leased...)
}

// personalKeyCtx is a router-keyed request whose key owns the given
// server-side subscription pools, as the auth middleware records it.
func personalKeyCtx(pools ...auth.SubscriptionProvider) context.Context {
	enrolled := make(map[auth.SubscriptionProvider]struct{}, len(pools))
	for _, p := range pools {
		enrolled[p] = struct{}{}
	}
	ctx := context.WithValue(routerKeyedCtx(), proxy.ManagedSubscriptionProvidersContextKey{}, enrolled)
	return proxy.WithManagedSubscriptionUsage(ctx)
}

// keyOnlyClaudeCodeRequest is Claude Code carrying only its router key: no
// OAuth bearer, so the subscription pool is the only way to reach a vendor.
func keyOnlyClaudeCodeRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
}

func managedUsage(ctx context.Context) *proxy.ManagedSubscriptionUsage {
	usage, _ := ctx.Value(proxy.ManagedSubscriptionUsageContextKey{}).(*proxy.ManagedSubscriptionUsage)
	return usage
}

// On a deployment with no vendor API key, a key-only Claude Code turn from a
// personal key reaches the real scorer with its enrolled pools' providers and
// is served on the leased account.
func TestManagedPoolOnlyDeploymentServesKeyOnlyClaudeCodeTurns(t *testing.T) {
	t.Run("Claude pick served on the enrolled Claude account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(auth.SubscriptionProviderClaude)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(qualityBias(ctx, 1), []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.Equal(t, "claude-fable-5", decisions[0].Model)
		assert.Equal(t, []string{"claude-fable-5-1"}, stack.anthropic.served())
		oauth, _ := stack.anthropic.calls()
		assert.Equal(t, []bool{true}, oauth, "the turn goes out on the leased subscription credential")
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		require.NotNil(t, managedUsage(ctx))
		assert.True(t, managedUsage(ctx).Served)
		assert.Empty(t, stack.openAI.served())
	})

	t.Run("GPT pick served on the enrolled Codex account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(auth.SubscriptionProviderCodex)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(ctx, []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requests, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.Contains(t, requests[0].ExcludedModels, "gpt-5.4-nano", "the pool serves only Codex-covered OpenAI models")
		assert.Equal(t, "gpt-5.5", decisions[0].Model)
		assert.Equal(t, []string{"gpt-6.1-sol"}, stack.openAI.served(), "the mapped pick is served on its Codex target")
		oauth, _ := stack.openAI.calls()
		assert.Equal(t, []bool{true}, oauth, "the turn goes out on the leased ChatGPT credential")
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
		assert.True(t, managedUsage(ctx).Served)
		assert.Empty(t, stack.anthropic.served())
	})

	t.Run("drained pool falls back to the local model", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{drained: true}
		stack.svc.WithManagedSubscriptions(pool)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(qualityBias(personalKeyCtx(auth.SubscriptionProviderClaude), 1), []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Empty(t, stack.anthropic.served(), "no vendor call goes out without a leased credential")
		stack.local.mu.Lock()
		defer stack.local.mu.Unlock()
		assert.Len(t, stack.local.bodies, 1)
	})

	t.Run("a shared key without enrollment still has no vendor", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)

		err := stack.svc.ProxyMessages(qualityBias(personalKeyCtx(), 1), []byte(localMainLoopBody), httptest.NewRecorder(), keyOnlyClaudeCodeRequest())

		require.ErrorIs(t, err, cluster.ErrNoEligibleProvider)
		assert.Empty(t, pool.leases())
		assert.Empty(t, stack.anthropic.served())
		assert.Empty(t, stack.openAI.served())
	})

	t.Run("drained pool without a fallback fails cleanly", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{drained: true}
		stack.svc.WithManagedSubscriptions(pool).WithSubscriptionLocalFallback(proxy.SubscriptionLocalFallback{})

		err := stack.svc.ProxyMessages(qualityBias(personalKeyCtx(auth.SubscriptionProviderClaude), 1), []byte(localMainLoopBody), httptest.NewRecorder(), keyOnlyClaudeCodeRequest())

		require.ErrorIs(t, err, proxy.ErrSubscriptionPoolExhausted)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Empty(t, stack.anthropic.served(), "no vendor call goes out without a leased credential")
		stack.local.mu.Lock()
		defer stack.local.mu.Unlock()
		assert.Empty(t, stack.local.bodies)
	})
}

// keyOnlyCodexRequest is Codex CLI carrying only its router key.
func keyOnlyCodexRequest(ctx context.Context, path string) (context.Context, *http.Request) {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	r.Header.Set("User-Agent", "codex_cli_rs/0.147.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.5")
	return requestcontext.WithClientIdentity(ctx, proxy.ClientIdentityFromHeaders(r.Header)), r
}

func TestManagedPoolOnlyDeploymentServesKeyOnlyCodexTurns(t *testing.T) {
	t.Run("Responses turn served on the enrolled Codex account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx, r := keyOnlyCodexRequest(personalKeyCtx(auth.SubscriptionProviderCodex), "/v1/responses")
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), rec, r))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"gpt-6.1-sol"}, stack.openAI.served())
		oauth, _ := stack.openAI.calls()
		assert.Equal(t, []bool{true}, oauth)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
	})

	t.Run("Chat Completions turn served on the enrolled Codex account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx, r := keyOnlyCodexRequest(personalKeyCtx(auth.SubscriptionProviderCodex), "/v1/chat/completions")
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyOpenAIChatCompletion(ctx, []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`), rec, r))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"gpt-6.1-sol"}, stack.openAI.served())
		oauth, _ := stack.openAI.calls()
		assert.Equal(t, []bool{true}, oauth)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
	})

	t.Run("drained Codex pool without a fallback never calls OpenAI", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{drained: true}
		stack.svc.WithManagedSubscriptions(pool).WithSubscriptionLocalFallback(proxy.SubscriptionLocalFallback{})
		ctx, r := keyOnlyCodexRequest(personalKeyCtx(auth.SubscriptionProviderCodex), "/v1/responses")

		err := stack.svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), httptest.NewRecorder(), r)

		require.ErrorIs(t, err, proxy.ErrSubscriptionPoolExhausted)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
		assert.Empty(t, stack.openAI.served(), "no OpenAI call goes out without a leased credential")
	})
}

// keyOnlyForcedClaudeCodeRequest is a key-only Claude Code turn pinned with
// /force-model.
func keyOnlyForcedClaudeCodeRequest(model string) *http.Request {
	r := keyOnlyClaudeCodeRequest()
	r.Header.Set(proxy.ForceModelHeader, model)
	return r
}

var bothPools = []auth.SubscriptionProvider{auth.SubscriptionProviderClaude, auth.SubscriptionProviderCodex}

// A forced model whose provider only the request's enrolled pool can serve is
// dispatched on the leased account, not refused as an unconfigured provider.
func TestManagedPoolOnlyDeploymentServesForcedModels(t *testing.T) {
	t.Run("forced claude-opus-5-5 served on the enrolled Claude account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		var logs bytes.Buffer
		ctx := observability.WithLogger(personalKeyCtx(bothPools...), slog.New(slog.NewJSONHandler(&logs, nil)))
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(ctx, []byte(localMainLoopBody), rec, keyOnlyForcedClaudeCodeRequest("claude-opus-5-5")))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"claude-opus-5-5"}, stack.anthropic.served())
		oauth, _ := stack.anthropic.calls()
		assert.Equal(t, []bool{true}, oauth, "the forced turn goes out on the leased subscription credential")
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Equal(t, "account-claude", managedUsage(ctx).SubscriptionAccountID)
		assert.Empty(t, stack.openAI.served())
		assert.Regexp(t, `"msg":"Managed subscription account leased",[^\n]*"provider":"anthropic","model":"claude-opus-5-5","account_id_prefix":"account-"`, logs.String(),
			"operators can see which pool account served the turn")
		assert.NotContains(t, logs.String(), "pool-token-claude", "the leased token is never logged")
	})

	t.Run("forced gpt-6.1-sol served on the enrolled Codex account", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(bothPools...)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(ctx, []byte(localMainLoopBody), rec, keyOnlyForcedClaudeCodeRequest("gpt-6.1-sol")))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"gpt-6.1-sol"}, stack.openAI.served())
		oauth, _ := stack.openAI.calls()
		assert.Equal(t, []bool{true}, oauth, "the forced turn goes out on the leased ChatGPT credential")
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
		assert.Equal(t, "account-codex", managedUsage(ctx).SubscriptionAccountID)
		assert.Empty(t, stack.anthropic.served())
	})

	t.Run("forced model on a provider no pool serves never reaches that provider", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)

		_ = stack.svc.ProxyMessages(personalKeyCtx(auth.SubscriptionProviderClaude), []byte(localMainLoopBody), httptest.NewRecorder(), keyOnlyForcedClaudeCodeRequest("gpt-6.1-sol"))

		assert.Empty(t, stack.openAI.served(), "no OpenAI call goes out without a credential")
		assert.NotContains(t, pool.leases(), subscriptions.ProviderCodex)
	})

	t.Run("forced model on a drained pool never calls the vendor", func(t *testing.T) {
		stack := newShippedStack(t)
		pool := &poolLeaser{drained: true}
		stack.svc.WithManagedSubscriptions(pool).WithSubscriptionLocalFallback(proxy.SubscriptionLocalFallback{})

		err := stack.svc.ProxyMessages(personalKeyCtx(bothPools...), []byte(localMainLoopBody), httptest.NewRecorder(), keyOnlyForcedClaudeCodeRequest("claude-opus-5-5"))

		require.ErrorIs(t, err, proxy.ErrSubscriptionPoolExhausted)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Empty(t, stack.anthropic.served(), "no vendor call goes out without a leased credential")
	})
}

// centroidPicking points the real scorer at the first v0.75 centroid whose
// pick, with both pools' providers enabled, is model.
func centroidPicking(t *testing.T, stack *shippedStack, hasTools bool, quality float64, model string) {
	t.Helper()
	for k := range stack.bundle.Centroids.K {
		stack.embedder.set(stack.bundle.Centroids.Row(k))
		d, err := stack.spy.inner.Route(context.Background(), router.Request{
			EnabledProviders: map[string]struct{}{providers.ProviderOpenAI: {}, providers.ProviderAnthropic: {}},
			HasTools:         hasTools,
			RoutingKnobs:     &router.Overrides{QualityBias: &quality},
		})
		require.NoError(t, err)
		if d.Model == model {
			return
		}
	}
	t.Fatalf("no v0.75 centroid picks %s at quality %v", model, quality)
}

// With both pools enrolled and no vendor key, the real scorer's automatic
// main-loop picks are mapped and served on the leased accounts.
func TestManagedPoolOnlyDeploymentMapsAutomaticPicks(t *testing.T) {
	t.Run("claude-sonnet-5 pick mapped to claude-sonnet-5-5 then served locally", func(t *testing.T) {
		stack := newShippedStack(t)
		centroidPicking(t, stack, true, 0, "claude-sonnet-5")
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(qualityBias(personalKeyCtx(bothPools...), 0), []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.Equal(t, "claude-sonnet-5", decisions[0].Model)
		assert.Contains(t, rec.Body.String(), "substitute for claude-sonnet-5-5 (mapped from claude-sonnet-5)")
		assert.Empty(t, stack.anthropic.served())
		stack.local.mu.Lock()
		defer stack.local.mu.Unlock()
		assert.Len(t, stack.local.bodies, 1)
	})

	t.Run("claude-sonnet-5-5 served on the Claude account when the local model is excluded", func(t *testing.T) {
		stack := newShippedStack(t)
		centroidPicking(t, stack, true, 0, "claude-sonnet-5")
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(bothPools...)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(context.WithValue(qualityBias(ctx, 0), proxy.InstallationExcludedModelsContextKey{}, []string{shippedLocalModelID}), []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, []string{"claude-sonnet-5-5"}, stack.anthropic.served())
		oauth, _ := stack.anthropic.calls()
		assert.Equal(t, []bool{true}, oauth)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Equal(t, "account-claude", managedUsage(ctx).SubscriptionAccountID)
	})

	t.Run("gpt-5.5 pick mapped to gpt-6.1-sol on the Codex account", func(t *testing.T) {
		stack := newShippedStack(t)
		centroidPicking(t, stack, true, 1, "gpt-5.5")
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(bothPools...)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(qualityBias(ctx, 1), []byte(localMainLoopBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.Equal(t, "gpt-5.5", decisions[0].Model)
		assert.Equal(t, []string{"gpt-6.1-sol"}, stack.openAI.served())
		oauth, _ := stack.openAI.calls()
		assert.Equal(t, []bool{true}, oauth)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderCodex}, pool.leases())
		assert.Equal(t, "account-codex", managedUsage(ctx).SubscriptionAccountID)
	})

	// A classifier turn is dispatched unmapped, so a gpt-5.5 pick that only
	// its mapping made servable is routed again and served on the Claude pool.
	t.Run("classifier turn served unmapped on the Claude account", func(t *testing.T) {
		stack := newShippedStack(t)
		centroidPicking(t, stack, false, 0.5, "gpt-5.5")
		pool := &poolLeaser{}
		stack.svc.WithManagedSubscriptions(pool)
		ctx := personalKeyCtx(bothPools...)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyMessages(qualityBias(ctx, 0.5), []byte(classifierTurnBody), rec, keyOnlyClaudeCodeRequest()))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requests, decisions := stack.spy.routed()
		require.Len(t, decisions, 2)
		assert.Equal(t, "gpt-5.5", decisions[0].Model)
		assert.Contains(t, requests[1].ExcludedModels, "gpt-5.5")
		assert.Equal(t, providers.ProviderAnthropic, decisions[1].Provider)
		assert.Equal(t, []string{decisions[1].Model}, stack.anthropic.served(), "a classifier turn is not mapped")
		oauth, _ := stack.anthropic.calls()
		assert.Equal(t, []bool{true}, oauth)
		assert.Equal(t, []subscriptions.Provider{subscriptions.ProviderClaude}, pool.leases())
		assert.Empty(t, stack.openAI.served())
	})
}

// classifierTurnBody is a short tool-less call, which the turn detector
// classifies as a classifier turn.
const classifierTurnBody = `{"model":"claude-opus-5","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
