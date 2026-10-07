package proxy_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var opus5Decision = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5", Reason: "cluster"}

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
	cases := map[string]struct {
		body    string
		headers http.Header
		want    string
	}{
		"user-forced model": {body: pinTestBody, headers: http.Header{"X-Weave-Force-Model": []string{"claude-opus-5"}}, want: "claude-opus-5"},
		"classifier turn":   {body: classifierBody, want: "claude-opus-5"},
		// Compaction is hard-pinned to the deployment's utility model.
		"compaction turn": {body: compactionBody, want: "claude-haiku-4-5"},
	}
	mapping := proxy.ModelMapping{"claude-opus-5": "claude-opus-5-5", "claude-haiku-4-5": "claude-sonnet-5-5"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMidTierFixture(t, "test-map-unmapped", opus5Decision, false, nil)
			f.svc.WithModelMapping(mapping)

			rec := f.serve(t, authedCtx(uuid.New().String()), tc.body, tc.headers)

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
