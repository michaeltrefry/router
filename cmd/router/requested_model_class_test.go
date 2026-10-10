package main

import (
	"net/http/httptest"
	"testing"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/sessionpin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// midPick is the scorer lifting a low-tier request to a mid model.
var midPick = router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-5-5", Reason: "cluster"}

const requestedLowYAML = classOrderYAML + "requested_model_class: [low]\n"

func requestedClassService(t *testing.T, orderYAML string, pins sessionpin.Store) classOrderStack {
	t.Helper()
	return classOrderServiceWith(t, classOrderOpts{anthropic: &streamingAnthropic{}, scorer: &countingRouter{decision: midPick}, orderYAML: orderYAML, pins: pins})
}

// localSubstituteBody names claude-sonnet-4-6, a low-tier model.
func TestRequestedModelClass_LowRequestServedInTheLowClass(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantModel, wantClass string
	}{
		{"enabled", requestedLowYAML, "test-mco-mimo", "low"},
		{"not configured", classOrderYAML, "claude-opus-5-5", "mid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := requestedClassService(t, tc.yaml, nil)
			rec := httptest.NewRecorder()

			require.NoError(t, stack.svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), rec, paidRequest()))

			assert.Equal(t, tc.wantModel, rec.Header().Get(proxy.HeaderRouterModel))
			assert.Equal(t, tc.wantClass, rec.Header().Get(proxy.HeaderRouterModelClass))
		})
	}
}

func TestRequestedModelClass_MidRequestKeepsTheRoutersPick(t *testing.T) {
	stack := requestedClassService(t, requestedLowYAML, nil)
	rec := httptest.NewRecorder()
	body := `{"model":"claude-opus-5-5","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}`

	require.NoError(t, stack.svc.ProxyMessages(routerKeyedCtx(), []byte(body), rec, paidRequest()))

	assert.Equal(t, "claude-opus-5-5", rec.Header().Get(proxy.HeaderRouterModel))
}

// A forced model outranks the implied class, on the forcing request and on
// later turns of the session that carry only the saved force.
func TestRequestedModelClass_ForcedModelOutranksTheImpliedClass(t *testing.T) {
	stack := requestedClassService(t, requestedLowYAML, &memoryPins{pins: map[string]sessionpin.Pin{}})

	forced := httptest.NewRecorder()
	require.NoError(t, stack.svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), forced, claudeCodeRequest("claude-opus-5-5")))
	assert.Equal(t, "claude-opus-5-5", forced.Header().Get(proxy.HeaderRouterModel), "the forcing request")

	saved := httptest.NewRecorder()
	require.NoError(t, stack.svc.ProxyMessages(routerKeyedCtx(), []byte(localSubstituteBody), saved, claudeCodeRequest("")))
	assert.Equal(t, "claude-opus-5-5", saved.Header().Get(proxy.HeaderRouterModel), "a later turn under the saved force")
}

func TestLoadLocalModels_RejectsUnknownRequestedModelClass(t *testing.T) {
	path := writeLocalModelsFile(t, "requested_model_class: [cheap]\n")

	_, err := loadLocalModels(envFrom(map[string]string{localModelsFileEnv: path}), nil, nil, discardLogger())

	require.ErrorIs(t, err, errRequestedModelClass)
}
