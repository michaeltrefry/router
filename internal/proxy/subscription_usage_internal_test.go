package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/subscriptions"
)

// headerReportingUpstream replays recorded upstream response headers through the
// provider-adapter observer hook before answering, as real adapters do.
type headerReportingUpstream struct {
	*parityUpstream
	headers http.Header
}

func (u headerReportingUpstream) Proxy(ctx context.Context, decision router.Decision, prepared providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	providers.ObserveUpstreamHeaders(ctx, u.headers)
	return u.parityUpstream.Proxy(ctx, decision, prepared, w, r)
}

func recordedAnthropicUnifiedHeaders() http.Header {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "2026-10-06T15:00:00Z")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "1.0")
	h.Set("anthropic-ratelimit-unified-7d-reset", "2026-10-09T00:00:00Z")
	return h
}

// A Claude response carrying unified rate-limit headers that passes through
// /v1/messages must be readable afterwards by the caller presenting the token.
func TestSubscriptionUsage_ReflectsRecordedAnthropicResponse(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ingress := parityAnthropicIngress()
	upstream := headerReportingUpstream{parityUpstream: &parityUpstream{okBody: ingress.upstreamOK(false)}, headers: recordedAnthropicUnifiedHeaders()}
	service := ingress.parityService(upstream)
	service.WithUsageObserver(usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now }))

	recorder, request, body := ingress.request(t, false)
	request.Header.Set("Authorization", "Bearer "+ingress.token)
	require.NoError(t, ingress.call(service, ingress.subCtx(), body, recorder, request))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, upstream.subDispatches)

	readings, readAt, err := service.SubscriptionUsage(request.Header, nil)
	require.NoError(t, err)
	assert.Equal(t, now, readAt)
	require.Len(t, readings, 1)
	reading := readings[0]
	assert.Equal(t, subscriptions.ProviderClaude, reading.Provider)
	assert.Equal(t, SubscriptionUsageSourcePresented, reading.Source)
	assert.True(t, reading.Observed)
	assert.Equal(t, service.usageObserver.Key([]byte(ingress.token)), reading.CredentialKey)
	assert.NotContains(t, string(reading.CredentialKey), ingress.token)
	assert.Equal(t, now, reading.ObservedAt)
	assert.True(t, reading.Exhausted, "the weekly window is spent until its reset")
	require.Len(t, reading.Windows, 2)
	assert.Equal(t, "primary", reading.Windows[0].Name)
	assert.InDelta(t, 0.42, reading.Windows[0].Utilization, 1e-9)
	assert.Equal(t, 300, reading.Windows[0].WindowMinutes)
	assert.Equal(t, time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC), reading.Windows[0].ResetAt)
	assert.False(t, reading.Windows[0].Exhausted)
	assert.Equal(t, "secondary", reading.Windows[1].Name)
	assert.InDelta(t, 1.0, reading.Windows[1].Utilization, 1e-9)
	assert.Equal(t, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC), reading.Windows[1].ResetAt)
	assert.True(t, reading.Windows[1].Exhausted)
}

// Readings are addressed only by keys derived from what the caller presents or
// owns, so another credential's observation is never returned.
func TestSubscriptionUsage_ScopedToPresentedAndOwnedCredentials(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).WithUsageObserver(observer)
	snapshot := usage.Snapshot{Primary: usage.Window{UsedPercent: 0.3, WindowMinutes: 300}}
	observer.Record(observer.Key([]byte("sk-ant-oat01-other-caller")), snapshot)
	observer.Record(observer.Key([]byte("subscription-account:other-account")), snapshot)
	observer.Record(observer.Key([]byte("codex.jwt.token")), snapshot)
	observer.Record(observer.Key([]byte("subscription-account:mine")), snapshot)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer codex.jwt.token")
	headers.Set("ChatGPT-Account-ID", "acct-1")
	readings, _, err := service.SubscriptionUsage(headers, []ManagedSubscriptionAccount{{ID: "mine", Provider: auth.SubscriptionProviderClaude}, {ID: "unobserved", Provider: auth.SubscriptionProviderCodex}})
	require.NoError(t, err)
	require.Len(t, readings, 3)
	assert.Equal(t, subscriptions.ProviderCodex, readings[0].Provider)
	assert.True(t, readings[0].Observed)
	assert.Equal(t, "mine", readings[1].AccountID)
	assert.True(t, readings[1].Observed)
	assert.Equal(t, "unobserved", readings[2].AccountID)
	assert.Equal(t, subscriptions.ProviderCodex, readings[2].Provider)
	assert.False(t, readings[2].Observed)
	assert.Empty(t, readings[2].Windows)

	readings, _, err = service.SubscriptionUsage(http.Header{}, nil)
	require.NoError(t, err)
	assert.Empty(t, readings, "a caller presenting and owning nothing reads nothing")
}

func TestSubscriptionUsage_UnavailableWithoutObserver(t *testing.T) {
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	_, _, err := service.SubscriptionUsage(http.Header{}, nil)
	assert.ErrorIs(t, err, ErrSubscriptionUsageUnavailable)
}
