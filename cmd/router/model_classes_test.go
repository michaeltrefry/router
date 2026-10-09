package main

import (
	"context"
	"fmt"
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
	"weave-os/router/internal/router/cluster"
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
	return classOrderServiceWith(t, classOrderOpts{anthropic: anthropic, scorer: &countingRouter{decision: pick}, orderYAML: classOrderYAML})
}

type classOrderOpts struct {
	anthropic providers.Client
	openAI    providers.Client
	scorer    router.Router
	orderYAML string
	rotate    []catalog.Tier
}

func classOrderServiceWith(t *testing.T, o classOrderOpts) classOrderStack {
	t.Helper()
	mimo, qwen := newLocalUpstream(t), newLocalUpstream(t)
	path := writeLocalModelsFile(t, lowLocalEntryYAML("test-mco-mimo", mimo.baseURL)+lowLocalEntryYAML("test-mco-qwen", qwen.baseURL)+
		userModelTiersYAML+o.orderYAML)
	openAI := &streamingOpenAI{}
	var openAIClient providers.Client = openAI
	if o.openAI != nil {
		openAIClient = o.openAI
	}
	anthropic := o.anthropic
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: anthropic, providers.ProviderOpenAI: openAIClient}
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
	svc := proxy.NewService(o.scorer, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithModelClassOrder(cfg.modelClassOrder).
		WithClassRotation(o.rotate...)
	return classOrderStack{svc: svc, anthropic: anthropic, openAI: openAI, mimo: mimo, qwen: qwen}
}

var lowPick = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}

// classedCtx is what the model-class middleware attaches for class, members
// included, plus the installation's excluded models.
func (s classOrderStack) classedCtx(class catalog.Tier, excluded ...string) context.Context {
	ctx := context.WithValue(routerKeyedCtx(), proxy.ModelClassContextKey{}, class)
	if members := s.svc.ModelClassMembers(class); len(members) > 0 {
		set := map[string]struct{}{}
		for _, m := range members {
			set[m] = struct{}{}
		}
		ctx = context.WithValue(ctx, proxy.ModelClassMembersContextKey{}, set)
	}
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

			require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow, tc.excluded...), []byte(localSubstituteBody), rec, paidRequest()))

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

	err := stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow, every...), []byte(localSubstituteBody), httptest.NewRecorder(), paidRequest())

	var unavailable *proxy.ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Empty(t, stack.openAI.served())
}

// A local entry that fails before output passes the turn to the next entry.
func TestModelClassOrder_FailedLocalEntryPassesToTheNextEntry(t *testing.T) {
	stack := classOrderService(t, &streamingAnthropic{}, lowPick)
	stack.mimo.server.Close()
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow), []byte(localSubstituteBody), rec, paidRequest()))

	assert.Equal(t, "test-mco-qwen", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Len(t, stack.qwen.bodies, 1)
}

// A high-class pick that fails before output is rescued on the next high
// entry, which the scorer itself can never pick.
func TestModelClassOrder_HighFallsBackToAstra(t *testing.T) {
	anthropic := &overloadedAnthropic{}
	stack := classOrderService(t, anthropic, router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-fable-5-1", Reason: "cluster"})
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh), []byte(localSubstituteBody), rec, paidRequest()))

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

// failingOpenAI answers every dispatch with a retryable 503.
type failingOpenAI struct {
	mu     sync.Mutex
	models []string
}

func (*failingOpenAI) SupportsSubscriptions() bool { return true }

func (o *failingOpenAI) Proxy(_ context.Context, decision router.Decision, _ providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	o.mu.Lock()
	o.models = append(o.models, decision.Model)
	o.mu.Unlock()
	return &providers.UpstreamErrorResponse{Status: http.StatusServiceUnavailable, Headers: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(`{"error":{"message":"unavailable"}}`)}
}

func (*failingOpenAI) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

// exclusionRespectingRouter picks pick unless the request excludes it.
type exclusionRespectingRouter struct{ pick router.Decision }

func (r exclusionRespectingRouter) Route(_ context.Context, req router.Request) (router.Decision, error) {
	if _, excluded := req.ExcludedModels[r.pick.Model]; excluded {
		return router.Decision{}, fmt.Errorf("every candidate excluded: %w", cluster.ErrNoEligibleProvider)
	}
	return r.pick, nil
}

// A failing vendor entry passes the turn to the next entry, never to a low
// model off the list such as the requested model's baseline.
func TestModelClassOrder_FailedVendorEntryPassesToTheNextEntryNotOffList(t *testing.T) {
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: &streamingAnthropic{}, openAI: &failingOpenAI{}, scorer: &countingRouter{decision: lowPick},
		orderYAML: "model_classes:\n  low: [test-mco-mimo, test-mco-qwen, gpt-5.6-terra, claude-sonnet-5-5]\n",
	})
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow, "test-mco-mimo", "test-mco-qwen"), []byte(localSubstituteBody), rec, paidRequest()))

	assert.Equal(t, "claude-sonnet-5-5", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, []string{"claude-sonnet-5-5"}, stack.anthropic.(*streamingAnthropic).served(), "neither the baseline nor the router's pick serves")
}

// With Fable excluded the scorer finds nothing in the high class; the order
// serves Astra, which the scorer cannot pick.
func TestModelClassOrder_HighWithFableExcludedServesAstra(t *testing.T) {
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: &streamingAnthropic{},
		scorer:    exclusionRespectingRouter{pick: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-fable-5-1", Reason: "cluster"}},
		orderYAML: classOrderYAML,
	})
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh, "claude-fable-5-1"), []byte(localSubstituteBody), rec, paidRequest()))

	assert.Equal(t, "gpt-6-astra", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, []string{"gpt-6-astra"}, stack.openAI.served())
}

// Fable refused by the caller's Claude subscription passes to Astra.
func TestModelClassOrder_HighSubscriptionRefusalServesAstra(t *testing.T) {
	anthropic := &streamingAnthropic{oauthStatus: http.StatusTooManyRequests}
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: anthropic, scorer: &countingRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-fable-5-1", Reason: "cluster"}},
		orderYAML: classOrderYAML,
	})
	rec := httptest.NewRecorder()

	require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh), []byte(localSubstituteBody), rec, claudeCodeRequest("")))

	assert.Equal(t, "gpt-6-astra", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, []string{"gpt-6-astra"}, stack.openAI.served())
}

// A class-ordered turn carries no in-band badge: a title turn's output is
// parsed by the harness, and the response headers name the model and class.
func TestModelClassOrder_TurnsCarryNoBadge(t *testing.T) {
	for name, body := range map[string]string{"title": localTitleGenBody, "main loop": localSubstituteBody} {
		t.Run(name, func(t *testing.T) {
			stack := classOrderService(t, &streamingAnthropic{}, lowPick)
			rec := httptest.NewRecorder()

			require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow), []byte(body), rec, paidRequest()))

			assert.NotContains(t, rec.Body.String(), "Weave Router")
			assert.Equal(t, "test-mco-mimo", rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}

// When every listed entry has failed, no rescue reaches a low model off the
// list: the requested model's baseline and the router's pick stay unused.
func TestModelClassOrder_ExhaustedListNeverReachesAnOffListModel(t *testing.T) {
	anthropic := &streamingAnthropic{}
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: anthropic, openAI: &failingOpenAI{}, scorer: &countingRouter{decision: lowPick},
		orderYAML: "model_classes:\n  low: [test-mco-mimo, test-mco-qwen, gpt-5.6-terra]\n",
	})

	_ = stack.svc.ProxyMessages(stack.classedCtx(catalog.TierLow, "test-mco-mimo", "test-mco-qwen"), []byte(localSubstituteBody), httptest.NewRecorder(), paidRequest())

	assert.Empty(t, anthropic.served(), "claude-sonnet-4-6 (baseline) and claude-haiku-4-5 (pick) are off the list")
}

// The model-classes example boots: its model_classes entries carry their
// classes' tiers once its model_tiers apply.
func TestLoadLocalModels_ModelClassesExampleBoots(t *testing.T) {
	t.Cleanup(func() {
		catalog.RestoreTiers()
		catalog.UntierMappingSources("gpt-5.5", "claude-fable-5")
		catalog.UnregisterLocalModels("qwen3.8-flash-next", "mimo-v2.6-flash-rl")
		for _, id := range []string{"qwen3.8-flash-next", "mimo-v2.6-flash-rl"} {
			delete(providers.ProviderFamilies, providers.LocalProviderName(id))
			delete(providers.APIKeyEnvVars, providers.LocalProviderName(id))
		}
	})
	cfg, err := loadLocalModels(envFrom(map[string]string{
		localModelsFileEnv: "../../docs/model-classes.example.yaml", "LOCAL_QWEN_API_KEY": "s", "LOCAL_MIMO_API_KEY": "s",
	}), map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.NoError(t, err)
	assert.Len(t, cfg.lowTier.Targets, 2)
	assert.Equal(t, []string{"claude-fable-5-1", "gpt-6-astra"}, cfg.modelClassOrder[catalog.TierHigh])
	assert.Equal(t, "mimo-v2.6-flash-rl", cfg.modelClassOrder[catalog.TierLow][0])
	assert.Equal(t, catalog.TierMid, catalog.TierFor("gpt-5.5"), "the mapped source takes Sol's deployment tier")
}

// sessionBody is a main-loop turn whose first user message names a session.
func sessionBody(session string) []byte {
	return []byte(`{"model":"claude-sonnet-4-6","max_tokens":256,"system":"You are Claude Code.",` +
		`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"task ` + session + `"}]}`)
}

// A rotating class assigns each session one model of its list and keeps it;
// sessions spread across the list, and the scorer's pick is not used.
func TestClassRotation_SessionsSpreadAndStick(t *testing.T) {
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: &streamingAnthropic{},
		scorer:    &countingRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-fable-5-1", Reason: "cluster"}},
		orderYAML: classOrderYAML,
		rotate:    []catalog.Tier{catalog.TierHigh, catalog.TierMid},
	})
	served := map[string]string{}
	for i := 0; i < 12; i++ {
		session := fmt.Sprintf("s%d", i)
		rec := httptest.NewRecorder()
		require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh), sessionBody(session), rec, paidRequest()))
		served[session] = rec.Header().Get(proxy.HeaderRouterModel)
	}
	models := map[string]int{}
	for _, m := range served {
		models[m]++
	}
	assert.Contains(t, models, "claude-fable-5-1")
	assert.Contains(t, models, "gpt-6-astra", "sessions spread across the high list, not only the scorer's pick")

	for session, model := range served {
		rec := httptest.NewRecorder()
		require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh), sessionBody(session), rec, paidRequest()))
		assert.Equal(t, model, rec.Header().Get(proxy.HeaderRouterModel), "session %s keeps its model", session)
	}
}

// A rotated pick that fails before output passes to the next entry of the
// session's rotated list.
func TestClassRotation_FailedPickFallsThroughTheRotatedList(t *testing.T) {
	stack := classOrderServiceWith(t, classOrderOpts{
		anthropic: &overloadedAnthropic{}, scorer: &countingRouter{decision: lowPick},
		orderYAML: classOrderYAML, rotate: []catalog.Tier{catalog.TierHigh},
	})
	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		require.NoError(t, stack.svc.ProxyMessages(stack.classedCtx(catalog.TierHigh), sessionBody(fmt.Sprintf("f%d", i)), rec, paidRequest()))
		assert.Equal(t, "gpt-6-astra", rec.Header().Get(proxy.HeaderRouterModel), "every session ends on Astra while Fable fails")
	}
}

func TestLoadLocalModels_RejectsClassRotationWithoutAList(t *testing.T) {
	for name, block := range map[string]string{
		"unknown class": "model_classes:\n  high: [claude-fable-5-1]\nclass_rotation: [top]\n",
		"no list":       "model_classes:\n  high: [claude-fable-5-1]\nclass_rotation: [mid]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(catalog.RestoreTiers)
			path := writeLocalModelsFile(t, userModelTiersYAML+block)
			_, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path}),
				map[string]providers.Client{}, map[string]struct{}{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			require.ErrorIs(t, err, errClassRotationClass)
		})
	}
}
