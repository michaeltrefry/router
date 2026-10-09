package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
)

func lowLocalEntryYAML(id, baseURL string) string {
	return strings.Replace(localEntryYAML(id, baseURL, "LOCAL_TEST_KEY"), "tier: mid", "tier: low", 1)
}

func TestParseLocalModels_LowTierSubstitute(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := strings.Replace(localEntryYAML("lo1", "http://localhost:1/v1", "KEY_A"), "tier: mid", "tier: low", 1) +
		strings.Replace(localEntryYAML("lo2", "http://localhost:2/v1", "KEY_A"), "tier: mid", "tier: low", 1) +
		localEntryYAML("mid1", "http://localhost:3/v1", "KEY_A")
	parse := func(block string) (localModelsConfig, error) {
		return parseLocalModels(strings.NewReader("models:\n"+entries+block), env)
	}

	cfg, err := parse("low_tier_substitute:\n  models: [lo2, lo1]\n")
	require.NoError(t, err)
	assert.Equal(t, proxy.LowTierSubstitute{Targets: []proxy.LocalTarget{
		{Provider: providers.LocalProviderName("lo2"), Model: "lo2"},
		{Provider: providers.LocalProviderName("lo1"), Model: "lo1"},
	}}, cfg.lowTier, "configured order is kept")

	cfg, err = parse("low_tier_substitute:\n  models: [lo1]\n  enabled: false\n")
	require.NoError(t, err)
	assert.Empty(t, cfg.lowTier.Targets)

	for block, want := range map[string]error{
		"low_tier_substitute:\n  models: [claude-haiku-4-5]\n": errLowTierSubstituteModel,
		"low_tier_substitute:\n  models: [mid1]\n":             errLowTierSubstituteTier,
		"low_tier_substitute:\n  models: [lo1, lo1]\n":         errLowTierSubstituteDuplicate,
		"low_tier_substitute:\n  enabled: true\n":              errLowTierSubstituteEmpty,
	} {
		_, err := parse(block)
		assert.ErrorIs(t, err, want, block)
	}
}

// lowTierService boots two low-tier local models through the composition root
// behind an Anthropic normal target scoring every turn onto claude-haiku-4-5.
func lowTierService(t *testing.T, first, second string, firstURL, secondURL string) (*proxy.Service, *streamingAnthropic) {
	t.Helper()
	anthropicClient := &streamingAnthropic{}
	svc := lowTierServiceFor(t, first, second, firstURL, secondURL, anthropicClient,
		router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"})
	return svc, anthropicClient
}

// lowTierServiceFor is lowTierService with the normal target's client and the
// low-tier pick the scorer makes.
func lowTierServiceFor(t *testing.T, first, second string, firstURL, secondURL string, normal providers.Client, pick router.Decision, extraYAML ...string) *proxy.Service {
	t.Helper()
	path := writeLocalModelsFile(t, lowLocalEntryYAML(first, firstURL)+localHeaderTimeoutYAML+lowLocalEntryYAML(second, secondURL)+
		"low_tier_substitute:\n  models: ["+first+", "+second+"]\n"+strings.Join(extraYAML, ""))
	providerMap := map[string]providers.Client{pick.Provider: normal}
	keyed := map[string]struct{}{pick.Provider: {}}
	cfg, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(first, second)
		for _, id := range []string{first, second} {
			provider := providers.LocalProviderName(id)
			delete(providers.ProviderFamilies, provider)
			delete(providers.APIKeyEnvVars, provider)
		}
	})
	return proxy.NewService(&countingRouter{decision: pick}, providerMap, nil, false, nil, nil, false, pick.Provider, pick.Model, nil).
		WithDeploymentKeyedProviders(keyed).
		WithLowTierSubstitute(cfg.lowTier).
		WithSubscriptionLocalFallback(cfg.subscriptionFallback)
}

func TestLowTierSubstitute_FirstLocalServesLowPick(t *testing.T) {
	first, second := newLocalUpstream(t), newLocalUpstream(t)
	svc, anthropicClient := lowTierService(t, "test-lts-a1", "test-lts-a2", first.baseURL, second.baseURL)
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), rec, claudeCodeRequest("")))

	assert.Len(t, first.bodies, 1)
	assert.Empty(t, second.bodies)
	assert.Empty(t, anthropicClient.served())
	assert.Equal(t, "test-lts-a1", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, "low", rec.Header().Get(proxy.HeaderRouterModelClass))
}

// The first local model's failure before output moves the turn to the second.
func TestLowTierSubstitute_FailedLocalPassesTurnToNextLocal(t *testing.T) {
	for _, mode := range []string{"refused", "500", "stall"} {
		t.Run(mode, func(t *testing.T) {
			failing, second := newFailingLocal(t, mode), newLocalUpstream(t)
			svc, anthropicClient := lowTierService(t, "test-lts-b1-"+mode, "test-lts-b2-"+mode, failing.baseURL, second.baseURL)
			var logs bytes.Buffer
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyMessages(loggedCtx(&logs), []byte(localSubstituteBody), rec, claudeCodeRequest("")))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Len(t, second.bodies, 1, "the second local model serves the turn")
			assert.Empty(t, anthropicClient.served(), "the router's pick is not needed")
			line := logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
			assert.Equal(t, "test-lts-b1-"+mode, line["local_model"])
			assert.Equal(t, "test-lts-b2-"+mode, line["fallback_model"])
			done := logLine(t, &logs, "ProxyMessages complete")
			assert.Equal(t, "claude-haiku-4-5", done["substituted_from_model"], "session state keeps the router's pick")
		})
	}
}

// With both local models down, the router's own pick serves the turn.
// An unpriced requested model ("nb") has no baseline failover, so only the
// chain itself holds each local error for the next rescue; "bv" keeps one.
func TestLowTierSubstitute_BothLocalsFailServesRouterPick(t *testing.T) {
	bodies := map[string]string{
		"bv": localSubstituteBody,
		"nb": strings.Replace(localSubstituteBody, `"model":"claude-sonnet-4-6"`, `"model":"unpriced-client-model"`, 1),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			first, second := newFailingLocal(t, "500"), newFailingLocal(t, "500")
			svc, anthropicClient := lowTierService(t, "test-lts-c1-"+name, "test-lts-c2-"+name, first.baseURL, second.baseURL)
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(body), rec, claudeCodeRequest("")))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Positive(t, first.count())
			assert.Positive(t, second.count())
			assert.Equal(t, []string{"claude-haiku-4-5"}, anthropicClient.served())
			assert.Contains(t, rec.Body.String(), "normal route answer")
		})
	}
}

// A local model the request excludes is skipped at selection time.
func TestLowTierSubstitute_ExcludedFirstLocalSelectsSecond(t *testing.T) {
	first, second := newLocalUpstream(t), newLocalUpstream(t)
	svc, _ := lowTierService(t, "test-lts-d1", "test-lts-d2", first.baseURL, second.baseURL)
	ctx := context.WithValue(routerKeyedCtx(), proxy.InstallationExcludedModelsContextKey{}, []string{"test-lts-d1"})
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyMessages(ctx, []byte(localSubstituteBody), rec, claudeCodeRequest("")))

	assert.Empty(t, first.bodies)
	assert.Len(t, second.bodies, 1)
}

// Chat Completions ingress runs the same chain.
func TestLowTierSubstitute_ChatCompletionsBothLocalsFailServesRouterPick(t *testing.T) {
	first, second := newFailingLocal(t, "500"), newFailingLocal(t, "500")
	openAIClient := &streamingOpenAI{}
	svc := lowTierServiceFor(t, "test-lts-e1", "test-lts-e2", first.baseURL, second.baseURL, openAIClient,
		router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4.1-mini", Reason: "scored"})
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyOpenAIChatCompletion(routerKeyedCtx(), []byte(chatCompletionsTurn), rec, chatCompletionsRequest()))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, first.count())
	assert.Positive(t, second.count())
	assert.Equal(t, []string{"gpt-4.1-mini"}, openAIClient.served())
	assert.Contains(t, rec.Body.String(), "normal route answer")
}
