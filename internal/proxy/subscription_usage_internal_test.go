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

	readings, readAt, err := service.SubscriptionUsage(request.Header, "", nil)
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
	readings, _, err := service.SubscriptionUsage(headers, "", []ManagedSubscriptionAccount{{ID: "mine", Provider: auth.SubscriptionProviderClaude, Enabled: true}, {ID: "unobserved", Provider: auth.SubscriptionProviderCodex, Enabled: true}})
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

	readings, _, err = service.SubscriptionUsage(http.Header{}, "", nil)
	require.NoError(t, err)
	assert.Empty(t, readings, "a caller presenting and owning nothing reads nothing")
}

func TestSubscriptionUsage_UnavailableWithoutObserver(t *testing.T) {
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	_, _, err := service.SubscriptionUsage(http.Header{}, "", nil)
	assert.ErrorIs(t, err, ErrSubscriptionUsageUnavailable)
}

// A pass-through turn is readable by the router key that sent it, without the
// token, and by no other key; presenting the token too does not duplicate it.
func TestSubscriptionUsage_ObservedPassThroughScopedToRouterKey(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ingress := parityAnthropicIngress()
	upstream := headerReportingUpstream{parityUpstream: &parityUpstream{okBody: ingress.upstreamOK(false)}, headers: recordedAnthropicUnifiedHeaders()}
	service := ingress.parityService(upstream)
	service.WithUsageObserver(usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now }))

	recorder, request, body := ingress.request(t, false)
	request.Header.Set("Authorization", "Bearer "+ingress.token)
	ctx := context.WithValue(ingress.subCtx(), APIKeyIDContextKey{}, "key-a")
	require.NoError(t, ingress.call(service, ctx, body, recorder, request))
	require.Equal(t, 1, upstream.subDispatches)

	readings, _, err := service.SubscriptionUsage(http.Header{}, "key-a", nil)
	require.NoError(t, err)
	require.Len(t, readings, 1)
	assert.Equal(t, SubscriptionUsageSourceObserved, readings[0].Source)
	assert.Equal(t, subscriptions.ProviderClaude, readings[0].Provider)
	assert.Equal(t, service.usageObserver.Key([]byte(ingress.token)), readings[0].CredentialKey)
	assert.True(t, readings[0].Observed)
	assert.True(t, readings[0].Exhausted)

	readings, _, err = service.SubscriptionUsage(http.Header{}, "key-b", nil)
	require.NoError(t, err)
	assert.Empty(t, readings, "another router key must not read key-a's observed credential")

	readings, _, err = service.SubscriptionUsage(request.Header, "key-a", nil)
	require.NoError(t, err)
	require.Len(t, readings, 1, "a presented token already observed under the key is reported once")
	assert.Equal(t, SubscriptionUsageSourcePresented, readings[0].Source)
}

// Each router key remembers a bounded set of pass-through credentials and
// forgets one once its observation expires.
func TestObservedSubscriptions_BoundedAndEvictedWithObservation(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	observed := newObservedSubscriptions()
	var keys []usage.CredentialKey
	for i := range maxObservedSubscriptionsPerKey + 1 {
		key := observer.Key([]byte{byte(i)})
		keys = append(keys, key)
		observer.Record(key, usage.Snapshot{Primary: usage.Window{UsedPercent: 0.1, WindowMinutes: 300}})
		observed.add("key-a", observedSubscription{provider: subscriptions.ProviderClaude, key: key}, observer)
	}
	listed := observed.list("key-a", observer)
	require.Len(t, listed, maxObservedSubscriptionsPerKey)
	assert.Equal(t, keys[1], listed[0].key, "the least recently seen credential is dropped first")
	assert.Equal(t, keys[maxObservedSubscriptionsPerKey], listed[maxObservedSubscriptionsPerKey-1].key)

	now = now.Add(time.Hour)
	assert.Empty(t, observed.list("key-a", observer))
	assert.NotContains(t, observed.byKey, "key-a", "an expired key's index entry is evicted")
}

// Managed readings carry durable health: a future cooldown is exhaustion even
// unobserved, and a disabled or reconnect-required account is not routable.
func TestSubscriptionUsage_ManagedStateAndCooldown(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).
		WithUsageObserver(usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now }))
	cooldown := now.Add(2 * time.Hour)
	readings, _, err := service.SubscriptionUsage(http.Header{}, "", []ManagedSubscriptionAccount{
		{ID: "cooling", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateActive, CooldownUntil: cooldown},
		{ID: "lapsed", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateCooldown, CooldownUntil: now.Add(-time.Minute)},
		{ID: "stuck", Provider: auth.SubscriptionProviderCodex, Enabled: true, State: auth.SubscriptionAccountStateExhausted},
		{ID: "off", Provider: auth.SubscriptionProviderCodex, Enabled: false, State: auth.SubscriptionAccountStateDisabled},
		{ID: "relink", Provider: auth.SubscriptionProviderCodex, Enabled: true, State: auth.SubscriptionAccountStateReconnectRequired},
		{ID: "paused", Provider: auth.SubscriptionProviderCodex, Enabled: false, State: auth.SubscriptionAccountStateActive},
	})
	require.NoError(t, err)
	require.Len(t, readings, 6)
	cooling, lapsed, stuck, off, relink := readings[0], readings[1], readings[2], readings[3], readings[4]
	assert.False(t, readings[5].Routable, "a disabled flag blocks routing whatever the health state")
	assert.True(t, cooling.Routable)
	assert.False(t, cooling.Observed)
	assert.True(t, cooling.Exhausted, "a future cooldown blocks routing even without an observation")
	assert.Equal(t, cooldown, cooling.ResumesAt)
	assert.False(t, lapsed.Exhausted, "a passed cooldown re-admits the account")
	assert.True(t, stuck.Exhausted)
	assert.True(t, stuck.ResumesAt.IsZero(), "an exhausted state with no cooldown has no known resume")
	assert.False(t, off.Routable)
	assert.False(t, relink.Routable)
	assert.True(t, stuck.Routable)
}

// Paid overage serves but is billable, so it reads as exhausted until the
// unified reset.
func TestSubscriptionUsage_PaidOverageIsExhausted(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).WithUsageObserver(observer)
	reset := now.Add(3 * time.Hour)
	observer.Record(observer.Key([]byte("sk-ant-oat01-overage")), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.2, WindowMinutes: 300}, OverageInUse: true, UnifiedResetAt: reset,
	})
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat01-overage")
	readings, _, err := service.SubscriptionUsage(headers, "", nil)
	require.NoError(t, err)
	require.Len(t, readings, 1)
	assert.True(t, readings[0].OverageInUse)
	assert.True(t, readings[0].Exhausted)
	assert.Equal(t, reset, readings[0].ResumesAt)
	summary := SummarizeSubscriptionUsage(readings)
	assert.True(t, summary.AllExhausted)
	assert.Equal(t, reset, summary.ResumesAt)
}

func TestSummarizeSubscriptionUsage(t *testing.T) {
	early, late := time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	assert.Equal(t, SubscriptionUsageSummary{}, SummarizeSubscriptionUsage(nil), "nothing known is neither exhausted nor headroom")

	summary := SummarizeSubscriptionUsage([]SubscriptionUsageReading{
		{Routable: true, Observed: true, Exhausted: true, ResumesAt: late},
		{Routable: true, Exhausted: true, ResumesAt: early},
		{Routable: false, Observed: true},
	})
	assert.Equal(t, SubscriptionUsageSummary{KnownCredentials: 2, ObservedCredentials: 1, AllExhausted: true, ResumesAt: early}, summary)

	summary = SummarizeSubscriptionUsage([]SubscriptionUsageReading{
		{Routable: true, Observed: true, Exhausted: true, ResumesAt: late},
		{Routable: true},
	})
	assert.False(t, summary.AllExhausted, "an unexhausted routable credential is capacity")
	assert.Equal(t, late, summary.ResumesAt)
}

// A credential resumes only when its last block clears, and never at a known
// instant when any block has no reported end.
func TestSubscriptionUsage_ResumesAfterEveryBlockClears(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	observer := usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
	service := NewService(nil, nil, nil, false, nil, nil, false, "", "", nil).WithUsageObserver(observer)
	cooldown, windowReset := now.Add(2*time.Hour), now.Add(5*time.Hour)
	observer.Record(service.managedSubscriptionUsageKey("blocked-twice"), usage.Snapshot{Primary: usage.Window{UsedPercent: 1, WindowMinutes: 300, ResetAt: windowReset}})
	observer.Record(observer.Key([]byte("sk-ant-oat01-open-ended")), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 1, WindowMinutes: 300, ResetAt: windowReset}, OverageInUse: true,
	})
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat01-open-ended")
	readings, _, err := service.SubscriptionUsage(headers, "", []ManagedSubscriptionAccount{
		{ID: "blocked-twice", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateActive, CooldownUntil: cooldown},
	})
	require.NoError(t, err)
	require.Len(t, readings, 2)
	openEnded, blockedTwice := readings[0], readings[1]
	assert.True(t, openEnded.Exhausted)
	assert.True(t, openEnded.ResumesAt.IsZero(), "overage with no reported reset leaves the resume unknown")
	assert.True(t, blockedTwice.Exhausted)
	assert.Equal(t, windowReset, blockedTwice.ResumesAt, "the later of cooldown and window reset")
	assert.Equal(t, windowReset, SummarizeSubscriptionUsage(readings).ResumesAt)
}
