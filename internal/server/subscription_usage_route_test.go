package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/server"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const usageRouteClaudeToken = "sk-ant-oat01-usage-route-worker-token"

type usageRouteRouter struct{}

func (usageRouteRouter) Route(context.Context, router.Request) (router.Decision, error) {
	return router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "test"}, nil
}

// usageRouteUpstream answers like Anthropic for an OAuth subscription turn,
// replaying unified rate-limit headers through the adapter observer hook.
type usageRouteUpstream struct{ subscriptionDispatches int }

func (*usageRouteUpstream) IncludedOnlySubscriptions() bool { return true }

func (u *usageRouteUpstream) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil && creds.OAuth {
		u.subscriptionDispatches++
	}
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
	headers.Set("anthropic-ratelimit-unified-5h-reset", "2026-10-06T15:00:00Z")
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.4")
	headers.Set("anthropic-ratelimit-unified-7d-reset", "2026-10-09T00:00:00Z")
	providers.ObserveUpstreamHeaders(ctx, headers)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`)
	return nil
}

func (*usageRouteUpstream) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

type usageRouteBody struct {
	AllExhausted        bool       `json:"all_exhausted"`
	ResumesAt           *time.Time `json:"resumes_at"`
	KnownCredentials    int        `json:"known_credentials"`
	ObservedCredentials int        `json:"observed_credentials"`
	Credentials         []struct {
		Provider      string `json:"provider"`
		Source        string `json:"source"`
		CredentialKey string `json:"credential_key"`
		Observed      bool   `json:"observed"`
		Exhausted     bool   `json:"exhausted"`
		Windows       map[string]struct {
			Utilization float64 `json:"utilization"`
			Exhausted   bool    `json:"exhausted"`
		} `json:"windows"`
	} `json:"credentials"`
}

type usageRouteFixture struct {
	engine   *gin.Engine
	observer *usage.Observer
	upstream *usageRouteUpstream
}

func newUsageRouteFixture(t *testing.T) usageRouteFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	installation := &auth.Installation{ID: uuid.NewString(), ExternalID: "ext-usage"}
	repo := readKeyRepo{installation: installation, scopes: map[string]auth.APIKeyScope{"rk_worker_a": auth.ScopeRouting, "rk_worker_b": auth.ScopeRouting}}
	authSvc := auth.NewService(nil, repo, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)
	upstream := &usageRouteUpstream{}
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	proxySvc := proxy.NewService(usageRouteRouter{}, map[string]providers.Client{providers.ProviderAnthropic: upstream}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-sonnet-4-6", nil).
		WithUsageObserver(observer)
	engine := gin.New()
	server.RegisterWithFeatures(engine, authSvc, proxySvc, nil, nil, server.DeploymentModeSelfHosted, nil, nil, nil, nil, server.Features{})
	return usageRouteFixture{engine: engine, observer: observer, upstream: upstream}
}

func (f usageRouteFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	return recorder
}

func (f usageRouteFixture) usage(t *testing.T, routerKey string) (*httptest.ResponseRecorder, usageRouteBody) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/subscriptions/usage", nil)
	if routerKey != "" {
		request.Header.Set(auth.RouterKeyHeader, routerKey)
	}
	recorder := f.serve(request)
	var body usageRouteBody
	if recorder.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	}
	return recorder, body
}

// The production route table mounts the usage read behind router-key auth.
func TestSubscriptionUsageRouteRequiresRouterKey(t *testing.T) {
	f := newUsageRouteFixture(t)
	recorder, _ := f.usage(t, "")
	assert.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())

	recorder, body := f.usage(t, "rk_worker_a")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, body.Credentials)
	assert.Zero(t, body.KnownCredentials)
	assert.False(t, body.AllExhausted, "nothing known to this process is not exhaustion")
}

// A worker turn sent with router key A and a Claude Code OAuth token teaches the
// observer; a poller holding only router key A then reads those windows, and
// router key B does not.
func TestSubscriptionUsageRouteReturnsWindowsObservedUnderRouterKey(t *testing.T) {
	f := newUsageRouteFixture(t)
	turn := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"investigate the failing dispatch and report back"}]}`))
	turn.Header.Set(auth.RouterKeyHeader, "rk_worker_a")
	turn.Header.Set("Authorization", "Bearer "+usageRouteClaudeToken)
	turn.Header.Set("Content-Type", "application/json")
	turnRecorder := f.serve(turn)
	require.Equal(t, http.StatusOK, turnRecorder.Code, turnRecorder.Body.String())
	require.Equal(t, 1, f.upstream.subscriptionDispatches, "the turn must be served on the worker's subscription")

	recorder, body := f.usage(t, "rk_worker_a")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, body.Credentials, 1, recorder.Body.String())
	credential := body.Credentials[0]
	assert.Equal(t, "claude", credential.Provider)
	assert.Equal(t, proxy.SubscriptionUsageSourceObserved, credential.Source)
	assert.Equal(t, string(f.observer.Key([]byte(usageRouteClaudeToken))), credential.CredentialKey)
	assert.True(t, credential.Observed)
	assert.True(t, credential.Exhausted)
	assert.True(t, credential.Windows["primary"].Exhausted)
	assert.InDelta(t, 0.4, credential.Windows["secondary"].Utilization, 1e-9)
	assert.True(t, body.AllExhausted)
	assert.Equal(t, 1, body.KnownCredentials)
	assert.Equal(t, 1, body.ObservedCredentials)
	require.NotNil(t, body.ResumesAt)
	assert.Equal(t, time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC), *body.ResumesAt)
	assert.NotContains(t, recorder.Body.String(), usageRouteClaudeToken)

	recorder, body = f.usage(t, "rk_worker_b")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, body.Credentials, "another router key must not read this key's observed credentials")
	assert.False(t, body.AllExhausted)
}
