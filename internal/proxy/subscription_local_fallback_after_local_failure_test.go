package proxy_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatchOrder records which upstream each dispatch reached, in order.
type dispatchOrder struct {
	names []string
}

type orderedClient struct {
	providers.Client
	name  string
	order *dispatchOrder
}

func (c orderedClient) Proxy(ctx context.Context, d router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	c.order.names = append(c.order.names, c.name)
	return c.Client.Proxy(ctx, d, prep, w, r)
}

func (orderedClient) SupportsSubscriptions() bool { return true }

type afterLocalFailureFixture struct {
	svc      *proxy.Service
	upstream *subscriptionUpstream
	failing  *fakeProvider
	fallback *fakeProvider
	order    *dispatchOrder
	fbModel  string
}

// newAfterLocalFailureFixture substitutes a failing local model for the
// scorer's claude-sonnet-5 pick and installs the subscription fallback on a
// second, healthy local model, or on the failing one when sameModel is set.
func newAfterLocalFailureFixture(t *testing.T, name string, sameModel bool) afterLocalFailureFixture {
	t.Helper()
	sub := newSubscriptionFallbackFixture(t, "test-after-fail-fb-"+name, providers.ProviderAnthropic, "claude-sonnet-5", false, false, nil)
	sub.upstream.subErr = claudeLimit429

	failingID := "test-after-fail-mid-" + name
	failingProvider := providers.LocalProviderName(failingID)
	registerTestLocalModel(t, catalog.Model{
		ID:            failingID,
		Tier:          catalog.TierMid,
		ContextWindow: 32_000,
		ImageInput:    catalog.ImageInputUnsupported,
		Providers:     []catalog.ProviderBinding{{Provider: failingProvider, UpstreamID: "upstream-" + failingID}},
	})
	failing := &fakeProvider{proxyErr: &providers.UpstreamErrorResponse{Status: http.StatusBadGateway, Body: []byte(`{"error":{"type":"server_error","message":"local down"}}`)}}

	order := &dispatchOrder{}
	svc := proxy.NewService(
		&fakeRouter{decision: sonnet5Decision},
		map[string]providers.Client{
			providers.ProviderAnthropic: orderedClient{Client: sub.upstream, name: "subscription", order: order},
			sub.provider:                orderedClient{Client: sub.local, name: "fallback", order: order},
			failingProvider:             orderedClient{Client: failing, name: "failing", order: order},
		},
		nil, false, nil, sub.store, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{sub.provider: {}, failingProvider: {}})
	svc.WithMidTierSubstitute(proxy.MidTierSubstitute{Provider: failingProvider, Model: failingID})
	fb := proxy.SubscriptionLocalFallback{Provider: sub.provider, Model: sub.model}
	if sameModel {
		fb = proxy.SubscriptionLocalFallback{Provider: failingProvider, Model: failingID}
	}
	svc.WithSubscriptionLocalFallback(fb)
	return afterLocalFailureFixture{svc: svc, upstream: sub.upstream, failing: failing, fallback: sub.local, order: order, fbModel: sub.model}
}

type afterLocalFailureIngress func(t *testing.T, f afterLocalFailureFixture, ctx context.Context) (*httptest.ResponseRecorder, error)

var afterLocalFailureIngresses = map[string]afterLocalFailureIngress{
	"messages": func(t *testing.T, f afterLocalFailureFixture, ctx context.Context) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		return rec, f.svc.ProxyMessages(ctx, []byte(pinTestBody), rec, req)
	},
	"chat completions": func(t *testing.T, f afterLocalFailureFixture, ctx context.Context) (*httptest.ResponseRecorder, error) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
		body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"fix the build"}]}`
		return rec, f.svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, req)
	},
}

// A substituted turn whose local model fails goes to its normal target; when
// that target's subscription refuses it, a different subscription fallback
// model serves the turn.
func TestSubscriptionLocalFallback_AfterLocalFailureServesOtherLocalModel(t *testing.T) {
	for ingress, serve := range afterLocalFailureIngresses {
		t.Run(ingress, func(t *testing.T) {
			f := newAfterLocalFailureFixture(t, "other", false)
			var logs bytes.Buffer
			ctx := observability.WithLogger(claudeSubscriptionCtx(), slog.New(slog.NewJSONHandler(&logs, nil)))

			rec, err := serve(t, f, ctx)

			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), "served locally")
			require.NotEmpty(t, f.order.names)
			assert.Equal(t, "failing", f.order.names[0], "the substituted local model is tried first")
			assert.Contains(t, f.order.names, "subscription", "the normal target's subscription is tried next")
			assert.Equal(t, "fallback", f.order.names[len(f.order.names)-1], "the subscription fallback model serves the turn")
			assert.Contains(t, logs.String(), `"original_model":"claude-sonnet-5"`)
			assert.Contains(t, logs.String(), `"fallback_model":"`+f.fbModel+`"`)
		})
	}
}

// When the subscription fallback model is the local model that just failed,
// it is not dispatched again: the subscription's refusal surfaces.
func TestSubscriptionLocalFallback_AfterLocalFailureNeverRedispatchesFailedModel(t *testing.T) {
	for ingress, serve := range afterLocalFailureIngresses {
		t.Run(ingress, func(t *testing.T) {
			f := newAfterLocalFailureFixture(t, "same", true)

			rec, err := serve(t, f, claudeSubscriptionCtx())

			require.Error(t, err)
			assert.Equal(t, http.StatusTooManyRequests, rec.Code)
			require.NotEmpty(t, f.order.names)
			assert.Equal(t, "failing", f.order.names[0])
			normal := slices.Index(f.order.names, "subscription")
			require.Positive(t, normal, "the normal target's subscription is tried after the local failure")
			assert.NotContains(t, f.order.names[normal:], "failing", "the failed local model is never dispatched again")
			assert.Empty(t, f.fallback.proxyBodies)
		})
	}
}

// A normal target whose subscription was already read spent, with no paid
// key, goes to the subscription fallback model without contacting the vendor.
func TestSubscriptionLocalFallback_AfterLocalFailureObservedExhaustionSkipsVendor(t *testing.T) {
	f := newAfterLocalFailureFixture(t, "observed", false)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackClaudeToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer)

	rec, err := afterLocalFailureIngresses["messages"](t, f, claudeSubscriptionCtx())

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, f.upstream.subDispatches+f.upstream.paidDispatches, "the vendor never receives the prompt")
	require.NotEmpty(t, f.order.names)
	assert.Equal(t, "fallback", f.order.names[len(f.order.names)-1])
}
