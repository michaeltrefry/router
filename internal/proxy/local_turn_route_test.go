package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/turntype"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	probeBody = `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`
	recapBody = `{"model":"claude-opus-4-7","max_tokens":512,"messages":[` +
		`{"role":"user","content":"fix the build"},` +
		`{"role":"assistant","content":"done"},` +
		`{"role":"user","content":"The user stepped away and is coming back. Recap in under 40 words."}]}`
	exploreWithToolsBody = `{"model":"claude-opus-4-7","metadata":{"user_id":"subagent:Explore"},` +
		`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"list go files"}]}`
)

// localTurnFixture is a service with an Anthropic client, a registered local
// model, and a scorer that always picks claude-opus-4-7.
type localTurnFixture struct {
	svc       *proxy.Service
	scorer    *fakeRouter
	store     *fakePinStore
	local     *fakeProvider
	anthropic *fakeProvider
	model     string
	provider  string
}

func registerTestLocalModel(t *testing.T, model catalog.Model) {
	t.Helper()
	provider := model.Providers[0].Provider
	require.NoError(t, catalog.RegisterLocalModels(model))
	require.NoError(t, providers.RegisterLocalProvider(provider, "LOCAL_TURN_TEST_KEY"))
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(model.ID)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
}

func newLocalTurnFixture(t *testing.T, id string, route bool, mutate func(*catalog.Model)) localTurnFixture {
	t.Helper()
	provider := providers.LocalProviderName(id)
	model := catalog.Model{
		ID:            id,
		Tier:          catalog.TierMid,
		ContextWindow: 32_000,
		ImageInput:    catalog.ImageInputUnsupported,
		Providers:     []catalog.ProviderBinding{{Provider: provider, UpstreamID: "upstream-" + id}},
	}
	if mutate != nil {
		mutate(&model)
	}
	registerTestLocalModel(t, model)

	anthropicResp := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}
	localResp := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
	f := localTurnFixture{
		scorer:    &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster"}},
		store:     newFakePinStore(),
		local:     &fakeProvider{proxyResponse: localResp},
		anthropic: &fakeProvider{proxyResponse: anthropicResp},
		model:     id,
		provider:  provider,
	}
	f.svc = proxy.NewService(
		f.scorer,
		map[string]providers.Client{providers.ProviderAnthropic: f.anthropic, provider: f.local},
		nil, false, nil, f.store,
		false, // hardPinExplore off: sub-agent turns are scored unless routed locally
		providers.ProviderAnthropic, "claude-haiku-4-5",
		nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}, provider: {}})
	if route {
		f.svc.WithLocalTurnRoute(proxy.LocalTurnRoute{Provider: provider, Model: id, TurnTypes: proxy.DefaultLocalTurnTypes})
	}
	return f
}

func (f localTurnFixture) serve(t *testing.T, ctx context.Context, body string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	for k, v := range headers {
		httpReq.Header[k] = v
	}
	require.NoError(t, f.svc.ProxyMessages(ctx, []byte(body), rec, httpReq))
	return rec
}

func TestLocalTurnRoute_DefaultTurnTypesDispatchToLocalModel(t *testing.T) {
	cases := map[string]string{
		"sub-agent dispatch": exploreBody,
		"title generation":   titleGenBody,
		"probe":              probeBody,
		"recap":              recapBody,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLocalTurnFixture(t, "test-local-turns", true, nil)

			rec := f.serve(t, authedCtx(uuid.New().String()), body, nil)

			assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Equal(t, f.provider, rec.Header().Get(proxy.HeaderRouterProvider))
			assert.Len(t, f.local.proxyBodies, 1, "the local upstream serves the turn")
			assert.Empty(t, f.anthropic.proxyBodies)
			assert.Zero(t, f.scorer.routeCalls, "a locally routed turn bypasses the scorer")
		})
	}
}

func TestLocalTurnRoute_NonRoutableTurnTypesNeverDispatchLocally(t *testing.T) {
	cases := map[string]string{
		"main loop":  pinTestBody,
		"classifier": classifierBody,
		"compaction": compactionBody,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLocalTurnFixture(t, "test-local-never", false, nil)
			// Even a route that lists every turn type keeps these off the local model.
			f.svc.WithLocalTurnRoute(proxy.LocalTurnRoute{Provider: f.provider, Model: f.model, TurnTypes: []turntype.TurnType{
				turntype.MainLoop, turntype.ToolResult, turntype.Classifier, turntype.Compaction,
				turntype.SubAgentDispatch, turntype.TitleGen, turntype.Probe, turntype.Recap,
			}})

			rec := f.serve(t, authedCtx(uuid.New().String()), body, nil)

			assert.Empty(t, f.local.proxyBodies)
			assert.NotEqual(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}

// With the local model in the installation's excluded models, every turn type
// routes exactly as it does with no route configured.
func TestLocalTurnRoute_ExcludedLocalModelRoutesAsBefore(t *testing.T) {
	cases := map[string]string{
		"sub-agent dispatch": exploreBody,
		"title generation":   titleGenBody,
		"probe":              probeBody,
		"recap":              recapBody,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			const id = "test-local-excluded"
			baseline := newLocalTurnFixture(t, id, false, nil)
			want := baseline.serve(t, authedCtx(uuid.New().String()), body, nil)
			catalog.UnregisterLocalModels(id)
			delete(providers.ProviderFamilies, providers.LocalProviderName(id))

			f := newLocalTurnFixture(t, id, true, nil)
			ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.InstallationExcludedModelsContextKey{}, []string{id})
			got := f.serve(t, ctx, body, nil)

			assert.Empty(t, f.local.proxyBodies)
			assert.Equal(t, want.Header().Get(proxy.HeaderRouterModel), got.Header().Get(proxy.HeaderRouterModel))
			assert.Equal(t, want.Header().Get(proxy.HeaderRouterProvider), got.Header().Get(proxy.HeaderRouterProvider))
			assert.Equal(t, baseline.scorer.routeCalls, f.scorer.routeCalls)
		})
	}
}

// A title-generation turn served locally must not pin the conversation that
// follows it to the local model.
func TestLocalTurnRoute_UtilityTurnDoesNotPinMainLoop(t *testing.T) {
	f := newLocalTurnFixture(t, "test-local-nopin", true, nil)
	ctx := authedCtx(uuid.New().String())

	f.serve(t, ctx, titleGenBody, nil)
	rec := f.serve(t, ctx, pinTestBody, nil)

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	for _, pin := range f.store.upserts {
		assert.NotEqual(t, f.model, pin.Model, "no session pin may name the local model")
	}
}

func TestLocalTurnRoute_UserForceOutranksLocalRoute(t *testing.T) {
	f := newLocalTurnFixture(t, "test-local-forced-away", true, nil)

	rec := f.serve(t, authedCtx(uuid.New().String()), exploreBody, http.Header{"X-Weave-Force-Model": []string{"claude-sonnet-4-6"}})

	assert.Equal(t, "claude-sonnet-4-6", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Empty(t, f.local.proxyBodies)
}

func TestLocalTurnRoute_RequestTheModelCannotCarryRoutesAsBefore(t *testing.T) {
	t.Run("tool-bearing turn on a low tool-use model", func(t *testing.T) {
		f := newLocalTurnFixture(t, "test-local-lowtools", true, func(m *catalog.Model) { m.ToolUseQuality = catalog.ToolUseLow })

		rec := f.serve(t, authedCtx(uuid.New().String()), exploreWithToolsBody, nil)

		assert.Empty(t, f.local.proxyBodies)
		assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
	})
	t.Run("history beyond the context window", func(t *testing.T) {
		f := newLocalTurnFixture(t, "test-local-small", true, func(m *catalog.Model) { m.ContextWindow = 1 })

		rec := f.serve(t, authedCtx(uuid.New().String()), exploreBody, nil)

		assert.Empty(t, f.local.proxyBodies)
		assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
	})
}
