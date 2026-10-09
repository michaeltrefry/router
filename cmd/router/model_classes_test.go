package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
)

const classOrderYAML = `model_classes:
  high: [claude-fable-5-1, gpt-6-astra]
  mid: [claude-opus-5-5, gpt-6.1-sol]
  low: [test-mco-mimo, test-mco-qwen, gpt-5.6-terra, claude-sonnet-5-5, gpt-6-luna, claude-haiku-4-5]
`

// overloadedAnthropic answers every dispatch with a retryable 529.
type overloadedAnthropic struct {
	mu     sync.Mutex
	models []string
}

func (*overloadedAnthropic) SupportsSubscriptions() bool { return true }

func (a *overloadedAnthropic) Proxy(_ context.Context, decision router.Decision, _ providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	a.mu.Lock()
	a.models = append(a.models, decision.Model)
	a.mu.Unlock()
	return &providers.UpstreamErrorResponse{Status: 529, Headers: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)}
}

func (*overloadedAnthropic) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

type classOrderStack struct {
	svc       *proxy.Service
	anthropic providers.Client
	openAI    *streamingOpenAI
	mimo      *localUpstream
	qwen      *localUpstream
}

// classOrderService boots the user's tiers, two low local models and the
// class order through the composition root, scoring every turn onto pick.
func classOrderService(t *testing.T, anthropic providers.Client, pick router.Decision) classOrderStack {
	t.Helper()
	mimo, qwen := newLocalUpstream(t), newLocalUpstream(t)
	path := writeLocalModelsFile(t, lowLocalEntryYAML("test-mco-mimo", mimo.baseURL)+lowLocalEntryYAML("test-mco-qwen", qwen.baseURL)+
		userModelTiersYAML+classOrderYAML)
	openAI := &streamingOpenAI{}
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropic, providers.ProviderOpenAI: openAI}
	keyed := map[string]struct{}{providers.ProviderAnthropic: {}, providers.ProviderOpenAI: {}}
	cfg, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.RestoreTiers()
		catalog.UnregisterLocalModels("test-mco-mimo", "test-mco-qwen")
		for _, id := range []string{"test-mco-mimo", "test-mco-qwen"} {
			delete(providers.ProviderFamilies, providers.LocalProviderName(id))
			delete(providers.APIKeyEnvVars, providers.LocalProviderName(id))
		}
	})
	svc := proxy.NewService(&countingRouter{decision: pick}, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithModelClassOrder(cfg.modelClassOrder)
	return classOrderStack{svc: svc, anthropic: anthropic, openAI: openAI, mimo: mimo, qwen: qwen}
}

var lowPick = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}

func classedCtx(class catalog.Tier, excluded ...string) context.Context {
	ctx := context.WithValue(routerKeyedCtx(), proxy.ModelClassContextKey{}, class)
	return context.WithValue(ctx, proxy.InstallationExcludedModelsContextKey{}, excluded)
}

func paidRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
}

func TestModelClassOrder_LowServesTheFirstEntryThatCanTakeTheTurn(t *testing.T) {
	cases := []struct {
		name     string
		excluded []string
		want     string
	}{
		{"first local", nil, "test-mco-mimo"},
		{"second local", []string{"test-mco-mimo"}, "test-mco-qwen"},
		{"terra after both locals", []string{"test-mco-mimo", "test-mco-qwen"}, "gpt-5.6-terra"},
		{"sonnet after terra", []string{"test-mco-mimo", "test-mco-qwen", "gpt-5.6-terra"}, "claude-sonnet-5-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stack := classOrderService(t, &streamingAnthropic{}, lowPick)
			rec := httptest.NewRecorder()

			require.NoError(t, stack.svc.ProxyMessages(classedCtx(catalog.TierLow, tc.excluded...), []byte(localSubstituteBody), rec, paidRequest()))

			assert.Equal(t, tc.want, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Equal(t, "low", rec.Header().Get(proxy.HeaderRouterModelClass))
		})
	}
}

// No low model off the list serves a low-class request, even the router's pick.
func TestModelClassOrder_LowNeverServesAModelOffTheList(t *testing.T) {
	offList := router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4.1-mini", Reason: "cluster"}
	stack := classOrderService(t, &streamingAnthropic{}, offList)
	every := []string{"test-mco-mimo", "test-mco-qwen", "gpt-5.6-terra", "claude-sonnet-5-5", "gpt-6-luna", "claude-haiku-4-5"}

	err := stack.svc.ProxyMessages(classedCtx(catalog.TierLow, every...), []byte(localSubstituteBody), httptest.NewRecorder(), paidRequest())

	var unavailable *proxy.ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Empty(t, stack.openAI.served())
}

// A local entry that fails before output passes the turn to the next entry.
func TestModelClassOrder_FailedLocalEntryPassesToTheNextEntry(t *testing.T) {
	stack := classOrderService(t, &streamingAnthropic{}, lowPick)
	stack.mimo.server.Close()
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(classedCtx(catalog.TierLow), []byte(localSubstituteBody), rec, paidRequest()))

	assert.Equal(t, "test-mco-qwen", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Len(t, stack.qwen.bodies, 1)
}

// A high-class pick that fails before output is rescued on the next high
// entry, which the scorer itself can never pick.
func TestModelClassOrder_HighFallsBackToAstra(t *testing.T) {
	anthropic := &overloadedAnthropic{}
	stack := classOrderService(t, anthropic, router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-fable-5-1", Reason: "cluster"})
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(classedCtx(catalog.TierHigh), []byte(localSubstituteBody), rec, paidRequest()))

	assert.Contains(t, anthropic.models, "claude-fable-5-1")
	assert.Equal(t, []string{"gpt-6-astra"}, stack.openAI.served())
	assert.Equal(t, "high", rec.Header().Get(proxy.HeaderRouterModelClass))
}

func TestLoadLocalModels_RejectsModelClassEntryOfAnotherTier(t *testing.T) {
	for name, block := range map[string]string{
		"tier mismatch": "model_classes:\n  high: [claude-sonnet-5-5]\n",
		"unknown model": "model_classes:\n  low: [claude-nope]\n",
		"duplicate":     "model_classes:\n  low: [claude-haiku-4-5, claude-haiku-4-5]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(catalog.RestoreTiers)
			path := writeLocalModelsFile(t, userModelTiersYAML+block)
			_, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path}),
				map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "model classes:"), err.Error())
		})
	}
}
