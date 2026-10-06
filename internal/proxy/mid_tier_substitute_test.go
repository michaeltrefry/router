package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const midTierToolResultBody = `{
	"model":"claude-opus-4-7",
	"system":"sys",
	"tools":[{"name":"R","description":"read","input_schema":{"type":"object"}}],
	"messages":[
		{"role":"user","content":"original prompt"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"R","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}
	]
}`

// newMidTierFixture is a local-model fixture whose scorer picks decision and
// whose service substitutes the local model for mid-tier selections when
// substitute is true.
func newMidTierFixture(t *testing.T, id string, decision router.Decision, substitute bool, mutate func(*catalog.Model)) localTurnFixture {
	t.Helper()
	f := newLocalTurnFixture(t, id, false, mutate)
	f.scorer.decision = decision
	if substitute {
		f.svc.WithMidTierSubstitute(proxy.MidTierSubstitute{Provider: f.provider, Model: f.model})
	}
	return f
}

var sonnet5Decision = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-5", Reason: "cluster"}

// completionLine returns the decoded "ProxyMessages complete" log line.
func completionLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, `"ProxyMessages complete"`) {
			continue
		}
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &fields))
		return fields
	}
	t.Fatalf("no completion line logged")
	return nil
}

func TestMidTierSubstitute_SonnetSelectionDispatchesToLocalModel(t *testing.T) {
	f := newMidTierFixture(t, "test-mid-sub", sonnet5Decision, true, nil)
	var logs bytes.Buffer
	ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := f.serve(t, ctx, pinTestBody, nil)

	require.Len(t, f.local.proxyBodies, 1, "the local upstream serves the turn")
	assert.Empty(t, f.anthropic.proxyBodies)
	assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, f.provider, rec.Header().Get(proxy.HeaderRouterProvider))
	line := completionLine(t, &logs)
	assert.Equal(t, f.model, line["decision_model"])
	assert.Equal(t, "claude-sonnet-5", line["substituted_from_model"])
	assert.Equal(t, providers.ProviderAnthropic, line["substituted_from_provider"])
}

func TestMidTierSubstitute_DispatchUnchanged(t *testing.T) {
	cases := map[string]struct {
		decision   router.Decision
		substitute bool
	}{
		"substitution disabled": {decision: sonnet5Decision, substitute: false},
		"high-tier selection": {
			decision:   router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5", Reason: "cluster"},
			substitute: true,
		},
		"low-tier selection": {
			decision:   router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"},
			substitute: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMidTierFixture(t, "test-mid-unchanged", tc.decision, tc.substitute, nil)
			var logs bytes.Buffer
			ctx := observability.WithLogger(authedCtx(uuid.New().String()), slog.New(slog.NewJSONHandler(&logs, nil)))

			rec := f.serve(t, ctx, pinTestBody, nil)

			assert.Empty(t, f.local.proxyBodies)
			assert.Len(t, f.anthropic.proxyBodies, 1)
			assert.Equal(t, tc.decision.Model, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Empty(t, completionLine(t, &logs)["substituted_from_model"])
		})
	}
}

func TestMidTierSubstitute_UserForceIsNeverSubstituted(t *testing.T) {
	f := newMidTierFixture(t, "test-mid-forced", sonnet5Decision, true, nil)

	rec := f.serve(t, authedCtx(uuid.New().String()), pinTestBody, http.Header{"X-Weave-Force-Model": []string{"claude-sonnet-5"}})

	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Empty(t, f.local.proxyBodies)
}

// The session pin keeps the scorer's own pick, so a tool-result turn that
// follows the pin is substituted again and the session stays on one model.
func TestMidTierSubstitute_PinKeepsOriginalAndFollowUpStaysLocal(t *testing.T) {
	f := newMidTierFixture(t, "test-mid-pin", sonnet5Decision, true, nil)
	f.store.persistUpserts = true
	ctx := authedCtx(uuid.New().String())

	f.serve(t, ctx, pinTestBody, nil)
	f.scorer.decision = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5", Reason: "cluster"}
	rec := f.serve(t, ctx, midTierToolResultBody, nil)

	assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Len(t, f.local.proxyBodies, 2)
	assert.Empty(t, f.anthropic.proxyBodies)
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	require.NotEmpty(t, f.store.upserts)
	for _, pin := range f.store.upserts {
		assert.NotEqual(t, f.model, pin.Model, "the session pin names the scorer's pick, never the substitute")
	}
	assert.Equal(t, "claude-sonnet-5", f.store.pin.Model)
}

// Turning substitution off returns a substituted session to the pinned
// model on its next turn.
func TestMidTierSubstitute_DisablingReturnsPinnedSessionToOriginal(t *testing.T) {
	f := newMidTierFixture(t, "test-mid-off", sonnet5Decision, true, nil)
	f.store.hasPin = true
	f.store.pin = sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		Reason:          "cluster",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastServedModel: f.model,
	}
	f.svc.WithMidTierSubstitute(proxy.MidTierSubstitute{})

	rec := f.serve(t, authedCtx(uuid.New().String()), midTierToolResultBody, nil)

	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Empty(t, f.local.proxyBodies)
}

func TestMidTierSubstitute_IneligibleLocalModelKeepsSelection(t *testing.T) {
	t.Run("installation excluded the local model", func(t *testing.T) {
		f := newMidTierFixture(t, "test-mid-excluded", sonnet5Decision, true, nil)
		ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.InstallationExcludedModelsContextKey{}, []string{f.model})

		rec := f.serve(t, ctx, pinTestBody, nil)

		assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
		assert.Empty(t, f.local.proxyBodies)
	})
	t.Run("history beyond the context window", func(t *testing.T) {
		f := newMidTierFixture(t, "test-mid-small", sonnet5Decision, true, func(m *catalog.Model) { m.ContextWindow = 1 })

		rec := f.serve(t, authedCtx(uuid.New().String()), pinTestBody, nil)

		assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
		assert.Empty(t, f.local.proxyBodies)
	})
	t.Run("tool-bearing turn on a low tool-use model", func(t *testing.T) {
		f := newMidTierFixture(t, "test-mid-lowtools", sonnet5Decision, true, func(m *catalog.Model) { m.ToolUseQuality = catalog.ToolUseLow })

		rec := f.serve(t, authedCtx(uuid.New().String()), midTierToolResultBody, nil)

		assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
		assert.Empty(t, f.local.proxyBodies)
	})
}

func TestMidTierSubstitute_ClassifierTurnIsNotSubstituted(t *testing.T) {
	f := newMidTierFixture(t, "test-mid-classifier", sonnet5Decision, true, nil)

	rec := f.serve(t, authedCtx(uuid.New().String()), classifierBody, nil)

	assert.Equal(t, "claude-sonnet-5", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Empty(t, f.local.proxyBodies)
}
