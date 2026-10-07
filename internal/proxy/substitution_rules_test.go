package proxy_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
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

// newRuleFixture serves decision through the substitution rules matching
// each pattern onto the fixture's local model, after mapping.
func newRuleFixture(t *testing.T, id string, decision router.Decision, mapping proxy.ModelMapping, patterns ...string) localTurnFixture {
	t.Helper()
	f := newMidTierFixture(t, id, decision, false, nil)
	rules := make([]proxy.SubstitutionRule, 0, len(patterns))
	for _, p := range patterns {
		rules = append(rules, proxy.SubstitutionRule{Match: p, Provider: f.provider, Model: f.model})
	}
	f.svc.WithModelMapping(mapping).WithSubstitutionRules(rules)
	return f
}

func loggedAuthedCtx(logs *bytes.Buffer) context.Context {
	return observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(logs, nil)))
}

// A rule matches the final model: the mapped target when a mapping applied,
// else the scorer's pick. The marker names the model the rule replaced.
func TestSubstitutionRule_MatchesTheFinalModel(t *testing.T) {
	cases := map[string]struct {
		mapping    proxy.ModelMapping
		pattern    string
		wantLocal  bool
		wantModel  string // when not local
		wantMarker string // when local
	}{
		"unmapped pick matches a glob": {
			pattern: "claude-opus-*", wantLocal: true,
			wantMarker: "substitute for claude-opus-5",
		},
		"mapped target matches": {
			mapping: defaultTestMapping, pattern: "claude-opus-5-5", wantLocal: true,
			wantMarker: "substitute for claude-opus-5-5 (mapped from claude-opus-5)",
		},
		"only the pre-mapping pick matches": {
			mapping: defaultTestMapping, pattern: "claude-opus-5", wantModel: "claude-opus-5-5",
		},
		"no rule matches": {pattern: "gpt-*-luna", wantModel: "claude-opus-5"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-rule-final", opus5Decision, tc.mapping, tc.pattern)
			var logs bytes.Buffer

			rec := f.serve(t, loggedAuthedCtx(&logs), pinTestBody, nil)

			line := completionLine(t, &logs)
			if !tc.wantLocal {
				assert.Empty(t, f.local.proxyBodies)
				require.Len(t, f.anthropic.proxyBodies, 1)
				assert.Equal(t, tc.wantModel, upstreamModel(t, f.anthropic.proxyBodies[0]))
				assert.NotEqual(t, "substitution_rule", line["substitution_reason"])
				return
			}
			require.Len(t, f.local.proxyBodies, 1)
			assert.Empty(t, f.anthropic.proxyBodies)
			assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Equal(t, "substitution_rule", line["substitution_reason"])
			assert.Equal(t, "claude-opus-5", line["substituted_from_model"], "session state keeps the scorer's pick")
			assert.Contains(t, line["routing_marker"], "→ "+f.model+" (local) · "+tc.wantMarker)
		})
	}
}

// Pattern rules are evaluated before the mid-tier substitute, which stays the
// last rule.
func TestSubstitutionRule_EvaluatedBeforeMidTierSubstitute(t *testing.T) {
	f := newRuleFixture(t, "test-rule-order", sonnet5Decision, nil, "claude-sonnet-*")
	f.svc.WithMidTierSubstitute(proxy.MidTierSubstitute{Provider: f.provider, Model: f.model})
	var logs bytes.Buffer

	f.serve(t, loggedAuthedCtx(&logs), pinTestBody, nil)

	require.Len(t, f.local.proxyBodies, 1)
	assert.Equal(t, "substitution_rule", completionLine(t, &logs)["substitution_reason"])
}

// A matched rule whose local model cannot take the request serves the matched
// model: the mapped target when mapped, else the scorer's pick.
func TestSubstitutionRule_IneligibleLocalServesMatchedModel(t *testing.T) {
	cases := map[string]struct {
		mapping proxy.ModelMapping
		pattern string
		want    string
	}{
		"mapped":   {mapping: defaultTestMapping, pattern: "claude-opus-5-5", want: "claude-opus-5-5"},
		"unmapped": {pattern: "claude-opus-5", want: "claude-opus-5"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-rule-inelig", opus5Decision, tc.mapping, tc.pattern)
			var logs bytes.Buffer
			ctx := context.WithValue(loggedAuthedCtx(&logs), proxy.InstallationExcludedModelsContextKey{}, []string{f.model})

			rec := f.serve(t, ctx, pinTestBody, nil)

			assert.Empty(t, f.local.proxyBodies)
			require.Len(t, f.anthropic.proxyBodies, 1)
			assert.Equal(t, tc.want, upstreamModel(t, f.anthropic.proxyBodies[0]))
			assert.Equal(t, tc.want, rec.Header().Get(proxy.HeaderRouterModel))
			skipped := logLine(t, &logs, "Local substitute skipped; serving the matched model")
			assert.Equal(t, "request_excluded", skipped["reason"])
		})
	}
}

// A rule-substituted turn whose local model fails before output is re-served
// on the matched model.
func TestSubstitutionRule_LocalFailureFallsBackToMatchedModel(t *testing.T) {
	cases := map[string]struct {
		mapping proxy.ModelMapping
		pattern string
		want    string
	}{
		"mapped":   {mapping: defaultTestMapping, pattern: "claude-opus-5-5", want: "claude-opus-5-5"},
		"unmapped": {pattern: "claude-opus-5", want: "claude-opus-5"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-rule-fail", opus5Decision, tc.mapping, tc.pattern)
			f.local.proxyErr = &providers.UpstreamErrorResponse{Status: http.StatusBadGateway, Body: []byte(`{"error":"down"}`)}
			var logs bytes.Buffer

			rec := f.serve(t, loggedAuthedCtx(&logs), pinTestBody, nil)

			require.NotEmpty(t, f.local.proxyBodies, "the substitute is tried first")
			require.Len(t, f.anthropic.proxyBodies, 1)
			assert.Equal(t, tc.want, upstreamModel(t, f.anthropic.proxyBodies[0]))
			assert.Equal(t, tc.want, rec.Header().Get(proxy.HeaderRouterModel))
			fallback := logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
			assert.Equal(t, "substitution_rule", fallback["local_source"])
		})
	}
}

// Pins and HMM history keep the scorer's pick, and a repeat of that pick is
// not a model switch: no routing marker on the second turn.
func TestSubstitutionRule_SessionStateKeepsScorerPick(t *testing.T) {
	cases := map[string]struct {
		mapping proxy.ModelMapping
		pattern string
	}{
		"unmapped":           {pattern: "claude-opus-5"},
		"mapped then matched": {mapping: defaultTestMapping, pattern: "claude-opus-5-5"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-rule-state", hmmSelection("claude-opus-5", 0.95), tc.mapping, tc.pattern)
			f.store.persistUpserts = true
			f.store.persistHMMHistory = true
			withUsageResponses(f)
			ctx := authedCtx(uuid.New().String())

			f.serve(t, ctx, pinTestBody, nil)
			var logs bytes.Buffer
			rec := f.serve(t, observability.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil))), midTierToolResultBody, nil)

			assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Len(t, f.local.proxyBodies, 2)
			assert.Empty(t, f.anthropic.proxyBodies)
			line := completionLine(t, &logs)
			assert.Equal(t, "claude-opus-5", line["prior_served_model"])
			assert.Empty(t, line["routing_marker"], "a second turn on the substitute is not a model switch")
			f.store.mu.Lock()
			defer f.store.mu.Unlock()
			assert.Equal(t, "claude-opus-5", f.store.hmmHistory.LastServedModel, "HMM history names the scorer's pick")
			require.NotEmpty(t, f.store.usages)
			for _, usage := range f.store.usages {
				assert.Equal(t, "claude-opus-5", usage.ServedModel, "last_served_model names the scorer's pick")
			}
		})
	}
}

// A repeat of a pinned pick is not a switch on the first rule-substituted turn
// either.
func TestSubstitutionRule_PinnedRepeatIsNotASwitch(t *testing.T) {
	f := newRuleFixture(t, "test-rule-repeat", opus5Decision, defaultTestMapping, "claude-opus-5-5")
	f.store.hasPin = true
	f.store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-opus-5",
		Reason:          "cluster",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastServedModel: "claude-opus-5",
	}
	var logs bytes.Buffer

	rec := f.serve(t, loggedAuthedCtx(&logs), mappedThinkingBody, nil)

	assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Empty(t, completionLine(t, &logs)["routing_marker"])
}

func TestSubstitutionRule_ForcedAndClassifierTurnsAreNotSubstituted(t *testing.T) {
	cases := map[string]struct {
		body    string
		headers http.Header
	}{
		"user-forced model": {body: pinTestBody, headers: http.Header{"X-Weave-Force-Model": []string{"claude-opus-5"}}},
		"classifier turn":   {body: classifierBody},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRuleFixture(t, "test-rule-skip", opus5Decision, nil, "claude-opus-5")

			rec := f.serve(t, authedCtx(uuid.New().String()), tc.body, tc.headers)

			assert.Empty(t, f.local.proxyBodies)
			assert.Equal(t, "claude-opus-5", rec.Header().Get(proxy.HeaderRouterModel))
		})
	}
}
