package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var opus5Decision = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5", Reason: "cluster"}

var testMappingPolicyPin = router.PolicyPin{
	ArtifactSHA256: strings.Repeat("a", 64),
	RosterSHA256:   strings.Repeat("b", 64),
}

var defaultTestMapping = proxy.ModelMapping{
	"claude-opus-5":   "claude-opus-5-5",
	"claude-sonnet-5": "claude-sonnet-5-5",
}

// upstreamModel decodes the model the upstream request body named.
func upstreamModel(t *testing.T, body []byte) string {
	t.Helper()
	var envelope struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope.Model
}

func TestModelMapping_ScorerPickDispatchesToMappedModel(t *testing.T) {
	f := newMidTierFixture(t, "test-map-opus", opus5Decision, false, nil)
	f.svc.WithModelMapping(defaultTestMapping)
	var logs bytes.Buffer
	ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := f.serve(t, ctx, pinTestBody, nil)

	require.Len(t, f.anthropic.proxyBodies, 1)
	assert.Equal(t, "claude-opus-5-5", upstreamModel(t, f.anthropic.proxyBodies[0]))
	assert.Equal(t, "claude-opus-5-5", rec.Header().Get(proxy.HeaderRouterModel))
	line := completionLine(t, &logs)
	assert.Equal(t, "claude-opus-5-5", line["decision_model"])
	assert.Equal(t, "claude-opus-5", line["substituted_from_model"])
	assert.Equal(t, "model_mapping", line["substitution_reason"])
	assert.Contains(t, line["routing_marker"], "→ claude-opus-5-5 · mapped from claude-opus-5")
}

func TestModelMapping_UnmappedDispatches(t *testing.T) {
	policyPinned := opus5Decision
	policyPinned.Metadata = &router.RoutingMetadata{PolicyPinHonoured: true}
	cases := map[string]struct {
		body     string
		headers  http.Header
		decision router.Decision
		ctx      func(context.Context) context.Context
		want     string
	}{
		"user-forced model": {body: pinTestBody, headers: http.Header{"X-Weave-Force-Model": []string{"claude-opus-5"}}, want: "claude-opus-5"},
		"classifier turn":   {body: classifierBody, want: "claude-opus-5"},
		// Compaction is hard-pinned to the deployment's utility model.
		"compaction turn": {body: compactionBody, want: "claude-haiku-4-5"},
		"honoured policy pin": {
			body:     pinTestBody,
			decision: policyPinned,
			ctx: func(ctx context.Context) context.Context {
				return router.WithPolicyPinRequest(ctx, router.PolicyPinRequest{Pin: testMappingPolicyPin, Authorized: true})
			},
			want: "claude-opus-5",
		},
	}
	mapping := proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5", "claude-haiku-4-5": "claude-sonnet-5-5"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			decision := opus5Decision
			if tc.decision.Model != "" {
				decision = tc.decision
			}
			f := newMidTierFixture(t, "test-map-unmapped", decision, false, nil)
			f.svc.WithModelMapping(mapping)
			ctx := authedCtx(uuid.New().String())
			if tc.ctx != nil {
				ctx = tc.ctx(ctx)
			}

			rec := f.serve(t, ctx, tc.body, tc.headers)

			require.Len(t, f.anthropic.proxyBodies, 1)
			assert.Equal(t, tc.want, upstreamModel(t, f.anthropic.proxyBodies[0]))
			assert.Equal(t, tc.want, rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}

// The session pin keeps the scorer's own pick, so the trained model stays the
// session's selection and every turn is mapped again.
func TestModelMapping_PinKeepsTrainedDecision(t *testing.T) {
	f := newMidTierFixture(t, "test-map-pin", opus5Decision, false, nil)
	f.svc.WithModelMapping(defaultTestMapping)
	f.store.persistUpserts = true
	withUsageResponses(f)
	ctx := authedCtx(uuid.New().String())

	f.serve(t, ctx, pinTestBody, nil)
	rec := f.serve(t, ctx, midTierToolResultBody, nil)

	assert.Equal(t, "claude-opus-5-5", rec.Header().Get(proxy.HeaderRouterModel))
	require.Len(t, f.anthropic.proxyBodies, 2)
	assert.Equal(t, "claude-opus-5-5", upstreamModel(t, f.anthropic.proxyBodies[1]))
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	require.NotEmpty(t, f.store.upserts)
	for _, pin := range f.store.upserts {
		assert.Equal(t, "claude-opus-5", pin.Model, "the session pin names the scorer's pick, never the mapped model")
	}
	for _, usage := range f.store.usages {
		assert.Equal(t, "claude-opus-5", usage.ServedModel)
	}
}

// Mapping runs before the mid-tier substitute, which still judges the tier of
// the scorer's pick: a mapped sonnet decision is served locally, and the
// session keeps the scorer's pick.
func TestModelMapping_MappedMidTierPickIsSubstituted(t *testing.T) {
	f := newMidTierFixture(t, "test-map-mid", sonnet5Decision, true, nil)
	f.svc.WithModelMapping(defaultTestMapping)
	var logs bytes.Buffer
	ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := f.serve(t, ctx, pinTestBody, nil)

	require.Len(t, f.local.proxyBodies, 1)
	assert.Empty(t, f.anthropic.proxyBodies)
	assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
	line := completionLine(t, &logs)
	assert.Equal(t, "claude-sonnet-5", line["substituted_from_model"])
	assert.Equal(t, "claude-sonnet-5-5", line["mapped_model"])
	assert.Equal(t, "mid_tier_substitute", line["substitution_reason"])
	assert.Contains(t, line["routing_marker"], "→ "+f.model+" (local) · substitute for claude-sonnet-5-5 (mapped from claude-sonnet-5)")
}

// A failed substitute for a mapped pick falls back to the mapped model, not
// to the scorer's unmapped pick.
func TestModelMapping_SubstituteFailureFallsBackToMappedModel(t *testing.T) {
	f := newMidTierFixture(t, "test-map-mid-fail", sonnet5Decision, true, nil)
	f.svc.WithModelMapping(defaultTestMapping)
	f.local.proxyErr = &providers.UpstreamErrorResponse{Status: http.StatusBadGateway, Body: []byte(`{"error":"down"}`)}

	rec := f.serve(t, authedCtx(uuid.New().String()), pinTestBody, nil)

	require.Len(t, f.anthropic.proxyBodies, 1)
	assert.Equal(t, "claude-sonnet-5-5", upstreamModel(t, f.anthropic.proxyBodies[0]))
	assert.Equal(t, "claude-sonnet-5-5", rec.Header().Get(proxy.HeaderRouterModel))
}

// A subscription refusal of the mapped model is served by the subscription
// fallback, and the turn is still recorded under the scorer's pick.
func TestModelMapping_SubscriptionFallbackKeepsTrainedDecision(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-map-sub-fb", providers.ProviderAnthropic, "claude-opus-5", false, true, nil)
	f.svc.WithModelMapping(defaultTestMapping)
	f.upstream.subErr = claudeLimit429
	var logs bytes.Buffer
	ctx := observability.WithLogger(claudeSubscriptionCtx(), slog.New(slog.NewJSONHandler(&logs, nil)))

	_, err := f.messages(t, ctx, pinTestBody)

	require.NoError(t, err)
	require.Len(t, f.local.proxyBodies, 1)
	line := completionLine(t, &logs)
	assert.Contains(t, line["routing_marker"], "fallback after claude-opus-5-5 subscription limit")
	assert.Equal(t, "claude-opus-5", line["substituted_from_model"])
	assert.Equal(t, "claude-opus-5-5", line["mapped_model"])
}

// mappedThinkingBody continues a tool loop whose last assistant turn carries a
// signed thinking block.
const mappedThinkingBody = `{
	"model":"claude-opus-4-7",
	"system":"sys",
	"max_tokens":512,
	"thinking":{"type":"adaptive"},
	"tools":[{"name":"R","description":"read","input_schema":{"type":"object"}}],
	"messages":[
		{"role":"user","content":"original prompt"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"prior thought","signature":"prior-turn-signature"},{"type":"tool_use","id":"t1","name":"R","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]
}`

// Session state records the router's own pick for a mapped or substituted
// turn, so a repeat of the same pick is not a model switch: no routing marker
// and, on the mapped Anthropic model, the signed thinking block is kept.
func TestServingRules_RepeatTurnIsNotASwitch(t *testing.T) {
	cases := map[string]struct {
		decision   router.Decision
		mapping    proxy.ModelMapping
		substitute bool
		wantModel  string // empty: the local substitute
	}{
		"mapped":                  {decision: opus5Decision, mapping: defaultTestMapping, wantModel: "claude-opus-5-5"},
		"substituted":             {decision: sonnet5Decision, substitute: true},
		"mapped then substituted": {decision: sonnet5Decision, mapping: defaultTestMapping, substitute: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMidTierFixture(t, "test-rules-repeat", tc.decision, tc.substitute, nil)
			f.svc.WithModelMapping(tc.mapping)
			f.store.hasPin = true
			f.store.pin = sessionpin.Pin{
				Provider:        tc.decision.Provider,
				Model:           tc.decision.Model,
				Reason:          tc.decision.Reason,
				PinnedUntil:     time.Now().Add(time.Hour),
				LastServedModel: tc.decision.Model,
			}
			var logs bytes.Buffer
			ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))

			rec := f.serve(t, ctx, mappedThinkingBody, nil)

			want := tc.wantModel
			if want == "" {
				want = f.model
			}
			assert.Equal(t, want, rec.Header().Get(proxy.HeaderRouterModel))
			line := completionLine(t, &logs)
			assert.Equal(t, tc.decision.Model, line["prior_served_model"])
			assert.Empty(t, line["routing_marker"], "a repeat of the session's pick is not news")
			if tc.wantModel == "" {
				require.Len(t, f.local.proxyBodies, 1)
				return
			}
			require.Len(t, f.anthropic.proxyBodies, 1)
			assert.Contains(t, string(f.anthropic.proxyBodies[0]), "prior-turn-signature", "the mapped model keeps its own signed reasoning")
		})
	}
}

// newMappingService serves the scorer's claude-opus-5 pick from a roster
// without the mapping targets, with Anthropic and, when withOpenAI, OpenAI
// dispatch clients registered.
func newMappingService(t *testing.T, mapping proxy.ModelMapping, withOpenAI bool) (*proxy.Service, *fakeProvider, *fakeProvider) {
	t.Helper()
	respond := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}
	anthropic := &fakeProvider{proxyResponse: respond}
	openai := &fakeProvider{proxyResponse: respond}
	clients := map[string]providers.Client{providers.ProviderAnthropic: anthropic}
	keyed := map[string]struct{}{providers.ProviderAnthropic: {}}
	if withOpenAI {
		clients[providers.ProviderOpenAI] = openai
		keyed[providers.ProviderOpenAI] = struct{}{}
	}
	svc := proxy.NewService(&fakeRouter{decision: opus5Decision}, clients, nil, false, nil, newFakePinStore(), false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(keyed).
		WithAvailableModels(map[string]struct{}{"claude-opus-5": {}, "claude-haiku-4-5": {}}).
		WithModelMapping(mapping)
	return svc, anthropic, openai
}

// A mapped target the request may not use leaves the scorer's pick in place.
func TestModelMapping_IneligibleTargetServesScorerPick(t *testing.T) {
	toOpus55 := proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5"}
	toSol := proxy.ModelMapping{"claude-opus-5": "gpt-6.1-sol"}
	cases := map[string]struct {
		mapping    proxy.ModelMapping
		withOpenAI bool
		key        any
		value      []string
	}{
		"installation excluded the target":       {mapping: toOpus55, key: proxy.InstallationExcludedModelsContextKey{}, value: []string{"claude-opus-5-5"}},
		"target outside the allowlist":           {mapping: toOpus55, key: proxy.InstallationAllowedModelsContextKey{}, value: []string{"claude-opus-5", "claude-haiku-4-5"}},
		"installation excluded the provider":     {mapping: toSol, withOpenAI: true, key: proxy.InstallationExcludedProvidersContextKey{}, value: []string{providers.ProviderOpenAI}},
		"target provider has no dispatch client": {mapping: toSol},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, anthropic, openai := newMappingService(t, tc.mapping, tc.withOpenAI)
			ctx := authedCtx(uuid.New().String())
			if tc.key != nil {
				ctx = context.WithValue(ctx, tc.key, tc.value)
			}
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil)))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Empty(t, openai.proxyBodies)
			require.Len(t, anthropic.proxyBodies, 1)
			assert.Equal(t, "claude-opus-5", upstreamModel(t, anthropic.proxyBodies[0]))
			assert.Equal(t, "claude-opus-5", rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}

// The eligibility cases above fail only for the reason they name: an eligible
// target on the same service is mapped.
func TestModelMapping_EligibleTargetIsMapped(t *testing.T) {
	cases := map[string]struct {
		mapping proxy.ModelMapping
		want    string
	}{
		"same provider":    {mapping: proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5"}, want: "claude-opus-5-5"},
		"another provider": {mapping: proxy.ModelMapping{"claude-opus-5": "gpt-6.1-sol"}, want: "gpt-6.1-sol"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, _, _ := newMappingService(t, tc.mapping, true)
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyMessages(authedCtx(uuid.New().String()), []byte(pinTestBody), rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil)))

			assert.Equal(t, tc.want, rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}
