package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func classCtx(class catalog.Tier) context.Context {
	return context.WithValue(authedCtx(uuid.New().String()), proxy.ModelClassContextKey{}, class)
}

// The fixture's local model is mid tier; a high-class request never moves a
// high pick onto it, and the response names the served class.
func TestModelClass_SubstitutionRuleNeverLeavesClass(t *testing.T) {
	f := newRuleFixture(t, "test-class-rule", opus5Decision, defaultTestMapping, "claude-opus-*")

	rec := f.serve(t, classCtx(catalog.TierHigh), pinTestBody, nil)

	assert.Empty(t, f.local.proxyBodies)
	require.Len(t, f.anthropic.proxyBodies, 1)
	assert.Equal(t, "claude-opus-5-5", upstreamModel(t, f.anthropic.proxyBodies[0]))
	assert.Equal(t, "high", rec.Header().Get(proxy.HeaderRouterModelClass))

	f = newRuleFixture(t, "test-class-rule-mid", opus5Decision, defaultTestMapping, "claude-opus-*")
	f.serve(t, authedCtx(uuid.New().String()), pinTestBody, nil)
	assert.Len(t, f.local.proxyBodies, 1, "without a class the rule still substitutes")
}

func TestModelClass_MidTierSubstituteNeverLeavesClass(t *testing.T) {
	f := newMidTierFixture(t, "test-class-mid-sub", sonnet5Decision, true, nil)

	f.serve(t, classCtx(catalog.TierHigh), pinTestBody, nil)

	assert.Empty(t, f.local.proxyBodies, "a mid local model never serves a high-class request")
}

// A mapping whose target is in another class serves the router's own pick.
func TestModelClass_CrossClassMappingServesTrainedModel(t *testing.T) {
	f := newRuleFixture(t, "test-class-map", opus5Decision, proxy.ModelMapping{"claude-opus-5": "claude-sonnet-5-5"})

	f.serve(t, classCtx(catalog.TierHigh), pinTestBody, nil)

	require.Len(t, f.anthropic.proxyBodies, 1)
	assert.Equal(t, "claude-opus-5", upstreamModel(t, f.anthropic.proxyBodies[0]))
}

// A title-gen turn the local route would take fails when nothing in the
// class can serve it, rather than running on the mid local model.
func TestModelClass_LocalTurnRouteNeverLeavesClass(t *testing.T) {
	f := newLocalTurnFixture(t, "test-class-turn-route", true, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := f.svc.ProxyMessages(classCtx(catalog.TierHigh), []byte(titleGenBody), httptest.NewRecorder(), req)

	var unavailable *proxy.ModelClassUnavailableError
	require.ErrorAs(t, err, &unavailable)
	assert.Empty(t, f.local.proxyBodies)
	assert.Empty(t, f.anthropic.proxyBodies)
}
