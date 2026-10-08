package main

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
)

// shippedScorerVersion is the trained bundle the shipped mapping targets.
const shippedScorerVersion = "v0.75"

// shippedLocalModelID is the local model docs/local-models.example.yaml declares.
const shippedLocalModelID = "qwen3.8-flash-next"

// fixedEmbedder stands in for the ONNX embedder so the real scorer embeds
// every prompt onto a chosen point of the bundle's embedding space.
type fixedEmbedder struct {
	mu  sync.Mutex
	vec []float32
	id  string
	dim int
}

func (e *fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.vec, nil
}
func (e *fixedEmbedder) ID() string   { return e.id }
func (e *fixedEmbedder) Dim() int     { return e.dim }
func (e *fixedEmbedder) Close() error { return nil }

func (e *fixedEmbedder) set(vec []float32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vec = vec
}

// scorerSpy records what the real scorer was asked and what it chose.
type scorerSpy struct {
	inner     router.Router
	mu        sync.Mutex
	requests  []router.Request
	decisions []router.Decision
}

func (s *scorerSpy) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	d, err := s.inner.Route(ctx, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	s.decisions = append(s.decisions, d)
	return d, err
}

func (s *scorerSpy) routed() ([]router.Request, []router.Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests), slices.Clone(s.decisions)
}

type shippedStack struct {
	svc       *proxy.Service
	openAI    *streamingOpenAI
	anthropic *streamingAnthropic
	local     *localUpstream
	spy       *scorerSpy
	embedder  *fixedEmbedder
	bundle    *cluster.Bundle
	cfg       localModelsConfig
}

// newShippedStack boots docs/local-models.example.yaml through the composition
// root's loader next to the real v0.75 cluster scorer. keyed names the
// providers holding a deployment key; every other provider is reachable only
// through the caller's subscription.
func newShippedStack(t *testing.T, keyed ...string) *shippedStack {
	t.Helper()
	local := newLocalUpstream(t)
	example, err := os.ReadFile(filepath.Join("..", "..", "docs", "local-models.example.yaml"))
	require.NoError(t, err)
	const exampleBaseURL = "http://localhost:8081/v1"
	require.Contains(t, string(example), exampleBaseURL)
	path := filepath.Join(t.TempDir(), "local-models.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(example), exampleBaseURL, local.baseURL, 1)), 0o600))

	openAI := &streamingOpenAI{}
	anthropic := &streamingAnthropic{}
	providerMap := map[string]providers.Client{providers.ProviderOpenAI: openAI, providers.ProviderAnthropic: anthropic}
	keyedSet := map[string]struct{}{}
	for _, p := range keyed {
		keyedSet[p] = struct{}{}
	}
	cfg, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_QWEN_API_KEY": "local-secret"}),
		providerMap, keyedSet, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(shippedLocalModelID)
		catalog.UntierMappingSources(slices.Collect(maps.Keys(cfg.modelMapping))...)
		provider := providers.LocalProviderName(shippedLocalModelID)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})

	available := make(map[string]struct{}, len(providerMap))
	for p := range providerMap {
		available[p] = struct{}{}
	}
	bundle, err := cluster.LoadBundle(shippedScorerVersion)
	require.NoError(t, err)
	embedder := &fixedEmbedder{id: bundle.EmbedderID(), dim: bundle.Centroids.Dim, vec: bundle.Centroids.Row(0)}
	scorer, err := cluster.NewScorer(bundle, cluster.DefaultConfig(), embedder, available)
	require.NoError(t, err)
	multi, err := cluster.NewMultiversion(shippedScorerVersion, map[string]*cluster.Scorer{shippedScorerVersion: scorer})
	require.NoError(t, err)
	require.NoError(t, validateModelMappingSelectable(cfg.modelMapping, multi.Default, multi.DefaultDeployedModels()))

	spy := &scorerSpy{inner: multi}
	svc := proxy.NewService(spy, providerMap, nil, false, nil, nil, false, providers.ProviderOpenAI, "gpt-5.6-luna", nil).
		WithDeploymentKeyedProviders(keyedSet).
		WithAvailableModels(catalog.RoutingTargetSet(available)).
		WithLocalTurnRoute(cfg.turnRoute).
		WithMidTierSubstitute(cfg.midTier).
		WithSubstitutionRules(cfg.substitutionRules).
		WithSubscriptionLocalFallback(cfg.subscriptionFallback).
		WithModelMapping(cfg.modelMapping)
	return &shippedStack{svc: svc, openAI: openAI, anthropic: anthropic, local: local, spy: spy, embedder: embedder, bundle: bundle, cfg: cfg}
}

// qualityBias returns ctx carrying the price/quality dial, a scorer input.
func qualityBias(ctx context.Context, q float64) context.Context {
	return router.WithRoutingKnobs(ctx, &router.Overrides{QualityBias: &q})
}

func candidateIDs(entries []cluster.DeployedEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Model)
	}
	return out
}

// Every shipped mapping source is a candidate of the real v0.75 scorer; the
// retired claude-fable-5 and gpt-5.5 are candidates only because the mapping
// gives them their target's tier.
func TestShippedMapping_SourcesAreRealScorerCandidates(t *testing.T) {
	stack := newShippedStack(t)
	scorer, err := cluster.NewScorer(stack.bundle, cluster.DefaultConfig(), stack.embedder,
		map[string]struct{}{providers.ProviderOpenAI: {}, providers.ProviderAnthropic: {}})
	require.NoError(t, err)

	candidates := candidateIDs(scorer.DeployedModels())
	for source := range stack.cfg.modelMapping {
		assert.Contains(t, candidates, source)
	}

	catalog.UntierMappingSources("claude-fable-5", "gpt-5.5")
	unmapped, err := cluster.NewScorer(stack.bundle, cluster.DefaultConfig(), stack.embedder,
		map[string]struct{}{providers.ProviderOpenAI: {}, providers.ProviderAnthropic: {}})
	require.NoError(t, err)
	assert.NotContains(t, candidateIDs(unmapped.DeployedModels()), "claude-fable-5", "without the mapping a retired row stays passthrough-only")
	assert.NotContains(t, candidateIDs(unmapped.DeployedModels()), "gpt-5.5")
}

// With only the caller's Codex subscription for OpenAI, the real scorer is
// offered gpt-5.4-mini and gpt-5.5; its gpt-5.4-mini pick is served by the
// local model through gpt-6-luna and its gpt-5.5 pick on gpt-6.1-sol.
func TestShippedMapping_CodexSubscriptionOnlyServesRealScorerPicks(t *testing.T) {
	cases := []struct {
		name  string
		ctx   func(context.Context) context.Context
		pick  string
		local bool
		codex []string
	}{
		{name: "gpt-5.4-mini pick served locally", ctx: func(ctx context.Context) context.Context { return qualityBias(ctx, 0) }, pick: "gpt-5.4-mini", local: true},
		{name: "gpt-5.5 pick served on gpt-6.1-sol", ctx: func(ctx context.Context) context.Context { return ctx }, pick: "gpt-5.5", codex: []string{"gpt-6.1-sol"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack := newShippedStack(t)
			ctx, r := codexRequest(t, nil)
			rec := httptest.NewRecorder()

			require.NoError(t, stack.svc.ProxyOpenAIResponses(tc.ctx(ctx), []byte(codexMainTurn), rec, r))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			requests, decisions := stack.spy.routed()
			require.Len(t, decisions, 1)
			assert.NotContains(t, requests[0].ExcludedModels, "gpt-5.4-mini", "the scorer request admits the mapped source")
			assert.NotContains(t, requests[0].ExcludedModels, "gpt-5.5")
			require.NotNil(t, decisions[0].Metadata)
			assert.Subset(t, decisions[0].Metadata.CandidateModels, []string{"gpt-5.4-mini", "gpt-5.5"})
			assert.Equal(t, tc.pick, decisions[0].Model)

			assert.Equal(t, tc.codex, stack.openAI.served())
			if len(tc.codex) > 0 {
				oauth, _ := stack.openAI.calls()
				assert.Equal(t, []bool{true}, oauth, "the mapped target runs on the caller's Codex subscription")
			}
			stack.local.mu.Lock()
			defer stack.local.mu.Unlock()
			if tc.local {
				require.Len(t, stack.local.bodies, 1, "gpt-6-luna matches the luna substitution rule")
				assert.Contains(t, rec.Body.String(), "substitute for gpt-6-luna (mapped from gpt-5.4-mini)")
				return
			}
			assert.Empty(t, stack.local.bodies)
		})
	}
}

// When the mapping cannot serve a Codex-subscription turn whose pick was
// admitted only for its mapped target, the uncovered pick is never sent: the
// turn is scored again without the mapping-admitted models. With nothing
// else eligible it fails as it would without the mapping; with a keyed
// provider it is served there.
func TestShippedMapping_CodexSubscriptionOnlyReroutesWhenMappingCannotServe(t *testing.T) {
	// gpt-5.4-mini is mid tier, so its unmapped pick reaches the local
	// mid-tier substitute, whose normal route would be the uncovered pick;
	// gpt-5.5 is dispatched unmapped directly.
	unservable := []struct {
		name     string
		ctx      func(context.Context) context.Context
		pick     string
		excluded string
	}{
		{name: "mid-tier pick, nothing else eligible", ctx: func(ctx context.Context) context.Context { return qualityBias(ctx, 0) }, pick: "gpt-5.4-mini", excluded: "gpt-6-luna"},
		{name: "high-tier pick, nothing else eligible", ctx: func(ctx context.Context) context.Context { return ctx }, pick: "gpt-5.5", excluded: "gpt-6.1-sol"},
	}
	for _, tc := range unservable {
		t.Run(tc.name, func(t *testing.T) {
			stack := newShippedStack(t)
			ctx, r := codexRequest(t, nil)
			ctx = context.WithValue(tc.ctx(ctx), proxy.InstallationExcludedModelsContextKey{}, []string{tc.excluded})

			err := stack.svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), httptest.NewRecorder(), r)

			require.ErrorIs(t, err, cluster.ErrNoEligibleProvider)
			requests, decisions := stack.spy.routed()
			require.Len(t, decisions, 2)
			assert.Equal(t, tc.pick, decisions[0].Model)
			assert.Contains(t, requests[1].ExcludedModels, "gpt-5.4-mini")
			assert.Contains(t, requests[1].ExcludedModels, "gpt-5.5")
			assert.Empty(t, stack.openAI.served(), "the uncovered pick is never dispatched on the ChatGPT credential")
			stack.local.mu.Lock()
			defer stack.local.mu.Unlock()
			assert.Empty(t, stack.local.bodies)
		})
	}

	t.Run("keyed Anthropic serves the reroute", func(t *testing.T) {
		stack := newShippedStack(t, providers.ProviderAnthropic)
		// Excluding the cheaper Claude models lets gpt-5.4-mini win the cheap
		// end of the dial against keyed Anthropic.
		excluded := []string{"gpt-6-luna", "claude-haiku-4-5", "claude-sonnet-5"}
		cheapest := 0.0
		var vec []float32
		for k := range stack.bundle.Centroids.K {
			stack.embedder.set(stack.bundle.Centroids.Row(k))
			d, err := stack.spy.inner.Route(context.Background(), router.Request{
				EnabledProviders: map[string]struct{}{providers.ProviderOpenAI: {}, providers.ProviderAnthropic: {}},
				ExcludedModels:   map[string]struct{}{"claude-haiku-4-5": {}, "claude-sonnet-5": {}},
				HasTools:         true,
				RoutingKnobs:     &router.Overrides{QualityBias: &cheapest},
			})
			require.NoError(t, err)
			if d.Model == "gpt-5.4-mini" {
				vec = stack.bundle.Centroids.Row(k)
				break
			}
		}
		require.NotNil(t, vec, "some v0.75 centroid picks gpt-5.4-mini on the cheap end of the dial")
		stack.embedder.set(vec)
		ctx, r := codexRequest(t, nil)
		ctx = context.WithValue(qualityBias(ctx, 0), proxy.InstallationExcludedModelsContextKey{}, excluded)
		rec := httptest.NewRecorder()

		require.NoError(t, stack.svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), rec, r))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requests, decisions := stack.spy.routed()
		require.Len(t, decisions, 2)
		assert.Equal(t, "gpt-5.4-mini", decisions[0].Model)
		assert.Contains(t, requests[1].ExcludedModels, "gpt-5.4-mini")
		assert.Equal(t, providers.ProviderAnthropic, decisions[1].Provider)
		assert.Empty(t, stack.openAI.served(), "the uncovered pick is never dispatched on the ChatGPT credential")
	})
}

// claudeSubscriptionRequest mirrors Claude Code on its Claude subscription
// with no force-model header.
func claudeSubscriptionRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer sk-ant-oat01-test-subscription")
	return r
}

// With only the caller's Claude subscription, the real scorer is offered every
// mapped Claude source, and its picks are served on the mapped targets.
func TestShippedMapping_ClaudeSubscriptionOnlyServesRealScorerPicks(t *testing.T) {
	cases := []struct {
		name   string
		ctx    func(context.Context) context.Context
		pick   string
		served string
	}{
		{name: "claude-fable-5 pick served on claude-fable-5-1", ctx: func(ctx context.Context) context.Context { return qualityBias(ctx, 1) }, pick: "claude-fable-5", served: "claude-fable-5-1"},
		{name: "claude-opus-5 pick served on claude-opus-5-5", ctx: func(ctx context.Context) context.Context { return ctx }, pick: "claude-opus-5", served: "claude-opus-5-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack := newShippedStack(t)
			rec := httptest.NewRecorder()

			require.NoError(t, stack.svc.ProxyMessages(tc.ctx(routerKeyedCtx()), []byte(localMainLoopBody), rec, claudeSubscriptionRequest()))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			requests, decisions := stack.spy.routed()
			require.Len(t, decisions, 1)
			for _, source := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5"} {
				assert.NotContains(t, requests[0].ExcludedModels, source)
			}
			require.NotNil(t, decisions[0].Metadata)
			assert.Subset(t, decisions[0].Metadata.CandidateModels, []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5"})
			assert.Equal(t, tc.pick, decisions[0].Model)
			assert.Equal(t, []string{tc.served}, stack.anthropic.served())
			oauth, _ := stack.anthropic.calls()
			assert.Equal(t, []bool{true}, oauth, "the mapped target runs on the caller's Claude subscription")
		})
	}
}

// Preferring a mapping target wins a routed turn against the real scorer: the
// target's mapping source is ranked, the scorer picks it where it otherwise
// would not, and the turn is served on the preferred target.
func TestShippedMapping_PreferredTargetWinsRealScorerTurn(t *testing.T) {
	const preferred, source = "claude-opus-5-5", "claude-opus-5"
	turnCtx := func(quality *float64, prefer bool) context.Context {
		ctx := routerKeyedCtx()
		if quality != nil {
			ctx = qualityBias(ctx, *quality)
		}
		if prefer {
			ctx = context.WithValue(ctx, proxy.InstallationPreferredModelsContextKey{}, []string{preferred})
		}
		return ctx
	}
	high, mid := 1.0, 0.5

	var vec []float32
	var quality *float64
	for _, q := range []*float64{nil, &high, &mid} {
		t.Run("probe", func(t *testing.T) {
			probe := newShippedStack(t)
			require.NoError(t, probe.svc.ProxyMessages(turnCtx(q, true), []byte(localMainLoopBody), httptest.NewRecorder(), claudeSubscriptionRequest()))
			requests, _ := probe.spy.routed()
			require.Len(t, requests, 1)
			withPreference := requests[0]
			require.Contains(t, withPreference.PreferredModels, source, "the preferred target ranks its mapping source")
			without := withPreference
			without.PreferredModels = nil
			for k := range probe.bundle.Centroids.K {
				probe.embedder.set(probe.bundle.Centroids.Row(k))
				base, err := probe.spy.inner.Route(context.Background(), without)
				require.NoError(t, err)
				pref, err := probe.spy.inner.Route(context.Background(), withPreference)
				require.NoError(t, err)
				if base.Model != source && pref.Model == source {
					vec, quality = probe.bundle.Centroids.Row(k), q
					return
				}
			}
		})
		if vec != nil {
			break
		}
	}
	require.NotNil(t, vec, "some v0.75 centroid and dial picks %s only when it is preferred", source)

	t.Run("without the preference the turn is served elsewhere", func(t *testing.T) {
		stack := newShippedStack(t)
		stack.embedder.set(vec)
		require.NoError(t, stack.svc.ProxyMessages(turnCtx(quality, false), []byte(localMainLoopBody), httptest.NewRecorder(), claudeSubscriptionRequest()))
		_, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.NotEqual(t, source, decisions[0].Model)
		assert.NotEqual(t, []string{preferred}, stack.anthropic.served())
	})

	t.Run("with the preference the turn is served on the preferred target", func(t *testing.T) {
		stack := newShippedStack(t)
		stack.embedder.set(vec)
		rec := httptest.NewRecorder()
		require.NoError(t, stack.svc.ProxyMessages(turnCtx(quality, true), []byte(localMainLoopBody), rec, claudeSubscriptionRequest()))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, decisions := stack.spy.routed()
		require.Len(t, decisions, 1)
		assert.Equal(t, source, decisions[0].Model)
		assert.Equal(t, []string{preferred}, stack.anthropic.served())
	})
}
