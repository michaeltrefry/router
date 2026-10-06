package proxy

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/subscriptions"
)

// Subscription usage reading sources.
const (
	// SubscriptionUsageSourcePresented is a subscription token the caller
	// presented on the reading request itself (proof of possession).
	SubscriptionUsageSourcePresented = "presented"
	// SubscriptionUsageSourceObserved is a pass-through subscription token this
	// process saw dispatched on an inference turn authenticated by the same
	// router API key. The reader holds only that key, not the token.
	SubscriptionUsageSourceObserved = "observed"
	// SubscriptionUsageSourceManaged is a router-managed account the caller's
	// verified subscription owner enrolled.
	SubscriptionUsageSourceManaged = "managed"
	// SubscriptionUsageSourceShared is another member's managed account the
	// caller may be dispatched onto. It carries state only: no account id.
	SubscriptionUsageSourceShared = "shared"
)

// ErrSubscriptionUsageUnavailable reports that no usage observer is wired, so
// an empty answer would falsely read as "no subscription is exhausted".
var ErrSubscriptionUsageUnavailable = errors.New("subscription usage observer is not configured")

// ManagedSubscriptionAccount is one managed account the caller can be
// dispatched onto, with its durable routing health.
type ManagedSubscriptionAccount struct {
	ID            string
	Provider      auth.SubscriptionProvider
	Shared        bool
	Enabled       bool
	State         auth.SubscriptionAccountState
	CooldownUntil time.Time
}

// SubscriptionUsageWindow is one observed rate-limit window: "primary" is the
// short rolling window (Claude 5h, Codex primary) and "secondary" the weekly one.
type SubscriptionUsageWindow struct {
	Name          string
	Utilization   float64
	WindowMinutes int
	ResetAt       time.Time
	Exhausted     bool
}

// SubscriptionUsageReading is the quota state of one credential the caller can
// be routed onto. It carries only the salted credential key, never a token.
// Observed is false when no fresh reading exists, which is not headroom.
// Exhausted means the router will not treat it as free capacity now (a spent
// window, paid overage, or a managed cooldown); ResumesAt is when every such
// block clears, zero when any block has no known end. Routable is false when
// the credential needs human action (disabled, reconnect) and never recovers
// on its own.
type SubscriptionUsageReading struct {
	Provider       subscriptions.Provider
	Source         string
	AccountID      string
	CredentialKey  usage.CredentialKey
	Routable       bool
	Enabled        bool
	State          auth.SubscriptionAccountState
	CooldownUntil  time.Time
	Observed       bool
	ObservedAt     time.Time
	Exhausted      bool
	ResumesAt      time.Time
	OverageInUse   bool
	UnifiedResetAt time.Time
	Windows        []SubscriptionUsageWindow
}

// SubscriptionUsageSummary folds readings into the poller's answer.
// KnownCredentials counts routable credentials; AllExhausted needs at least
// one, so "nothing known to this process" never reads as exhausted. ResumesAt
// is the earliest known instant an exhausted routable credential is usable.
type SubscriptionUsageSummary struct {
	KnownCredentials    int
	ObservedCredentials int
	AllExhausted        bool
	ResumesAt           time.Time
}

// SummarizeSubscriptionUsage aggregates readings over routable credentials.
func SummarizeSubscriptionUsage(readings []SubscriptionUsageReading) SubscriptionUsageSummary {
	var summary SubscriptionUsageSummary
	exhausted := 0
	for _, reading := range readings {
		if !reading.Routable {
			continue
		}
		summary.KnownCredentials++
		if reading.Observed {
			summary.ObservedCredentials++
		}
		if !reading.Exhausted {
			continue
		}
		exhausted++
		if !reading.ResumesAt.IsZero() && (summary.ResumesAt.IsZero() || reading.ResumesAt.Before(summary.ResumesAt)) {
			summary.ResumesAt = reading.ResumesAt
		}
	}
	summary.AllExhausted = summary.KnownCredentials > 0 && exhausted == summary.KnownCredentials
	return summary
}

// SubscriptionUsage returns quota readings for exactly the credentials the
// caller can be routed onto: subscription tokens presented in headers, pass-
// through tokens this process observed under the caller's router API key ID,
// and the managed accounts admitted for the caller. Readings for any other
// credential are unreachable because the observer is addressed only by keys
// derived here. now is the observer clock used to judge exhaustion.
func (s *Service) SubscriptionUsage(headers http.Header, apiKeyID string, managed []ManagedSubscriptionAccount) (readings []SubscriptionUsageReading, now time.Time, err error) {
	if s.usageObserver == nil {
		return nil, time.Time{}, ErrSubscriptionUsageUnavailable
	}
	now = s.usageObserver.Now()
	seen := map[usage.CredentialKey]struct{}{}
	passThrough := func(provider subscriptions.Provider, source string, key usage.CredentialKey) {
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		readings = append(readings, s.subscriptionUsageReading(SubscriptionUsageReading{Provider: provider, Source: source, CredentialKey: key, Routable: true, Enabled: true}, now))
	}
	if creds := ExtractClientCredentials(providers.ProviderAnthropic, headers); creds != nil && creds.OAuth && creds.Source == credSourceSubscription {
		passThrough(subscriptions.ProviderClaude, SubscriptionUsageSourcePresented, s.usageObserver.Key(creds.APIKey))
	}
	if creds := ExtractClientCredentials(providers.ProviderOpenAI, headers); creds != nil && creds.OAuth && creds.Source == credSourceCodexSubscription {
		passThrough(subscriptions.ProviderCodex, SubscriptionUsageSourcePresented, s.usageObserver.Key(creds.APIKey))
	}
	for _, observed := range s.observedSubscriptions.list(apiKeyID, s.usageObserver) {
		passThrough(observed.provider, SubscriptionUsageSourceObserved, observed.key)
	}
	for _, account := range managed {
		if account.ID == "" {
			continue
		}
		reading := SubscriptionUsageReading{
			Provider: subscriptions.Provider(account.Provider), Source: SubscriptionUsageSourceManaged, AccountID: account.ID,
			CredentialKey: s.managedSubscriptionUsageKey(account.ID), Enabled: account.Enabled, State: account.State, CooldownUntil: account.CooldownUntil,
			Routable: account.Enabled && managedStateRecovers(account.State),
		}
		if account.Shared {
			reading.Source, reading.AccountID = SubscriptionUsageSourceShared, ""
		}
		readings = append(readings, s.subscriptionUsageReading(reading, now))
	}
	return readings, now, nil
}

// managedStateRecovers reports whether the router may route the account now or
// once its cooldown passes; disabled and reconnect_required need a human.
func managedStateRecovers(state auth.SubscriptionAccountState) bool {
	return state == "" || state.Routable() || state == auth.SubscriptionAccountStateExhausted || state == auth.SubscriptionAccountStateCooldown
}

// subscriptionUsageReading fills the observed windows and the exhaustion
// verdict. Each block on the credential contributes its end; the credential
// resumes when the last one clears, unknown when any block has no end.
func (s *Service) subscriptionUsageReading(reading SubscriptionUsageReading, now time.Time) SubscriptionUsageReading {
	var resumes []time.Time
	block := func(until time.Time) {
		reading.Exhausted = true
		resumes = append(resumes, until)
	}
	// Mirrors the subscriptions admission rule: a future cooldown blocks any
	// state; an exhausted/cooldown state with no cooldown end never clears by time.
	if reading.CooldownUntil.After(now) {
		block(reading.CooldownUntil)
	} else if reading.CooldownUntil.IsZero() && (reading.State == auth.SubscriptionAccountStateExhausted || reading.State == auth.SubscriptionAccountStateCooldown) {
		block(time.Time{})
	}
	if snapshot, observed := s.usageObserver.Snapshot(reading.CredentialKey); observed {
		reading.Observed = true
		reading.ObservedAt = snapshot.ObservedAt
		reading.OverageInUse = snapshot.OverageInUse
		reading.UnifiedResetAt = snapshot.UnifiedResetAt
		if snapshot.OverageInUse {
			// Paid overage still serves, but the router treats it as billable.
			block(snapshot.UnifiedResetAt)
		}
		for _, window := range [...]struct {
			name string
			usage.Window
		}{{"primary", snapshot.Primary}, {"secondary", snapshot.Secondary}} {
			if !window.Reported() {
				continue
			}
			exhausted := window.Window.ExhaustedAsOf(now)
			if exhausted {
				block(window.ResetAt)
			}
			reading.Windows = append(reading.Windows, SubscriptionUsageWindow{Name: window.name, Utilization: window.UsedPercent, WindowMinutes: window.WindowMinutes, ResetAt: window.ResetAt, Exhausted: exhausted})
		}
	}
	for _, until := range resumes {
		if until.IsZero() {
			reading.ResumesAt = time.Time{}
			break
		}
		if until.After(reading.ResumesAt) {
			reading.ResumesAt = until
		}
	}
	return reading
}

// maxObservedSubscriptionsPerKey bounds how many pass-through credentials one
// router API key remembers; the least recently seen is dropped first.
const maxObservedSubscriptionsPerKey = 8

type observedSubscription struct {
	provider subscriptions.Provider
	key      usage.CredentialKey
}

// observedSubscriptions maps a router API key ID to the pass-through
// subscription credentials its inference turns were recorded under, so a
// poller holding only that router key can read them. Per-process memory, like
// the observer it indexes; an entry leaves once its observation expires.
type observedSubscriptions struct {
	mu    sync.Mutex
	byKey map[string][]observedSubscription
}

func newObservedSubscriptions() *observedSubscriptions {
	return &observedSubscriptions{byKey: make(map[string][]observedSubscription)}
}

// add records entry as the key's most recently seen credential, dropping
// entries whose observation has expired and the oldest beyond the bound.
func (o *observedSubscriptions) add(apiKeyID string, entry observedSubscription, observer *usage.Observer) {
	if o == nil || apiKeyID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var entries []observedSubscription
	for _, existing := range o.byKey[apiKeyID] {
		if existing.key == entry.key {
			continue
		}
		if _, live := observer.Snapshot(existing.key); live {
			entries = append(entries, existing)
		}
	}
	entries = append(entries, entry)
	if len(entries) > maxObservedSubscriptionsPerKey {
		entries = entries[len(entries)-maxObservedSubscriptionsPerKey:]
	}
	o.byKey[apiKeyID] = entries
}

// list returns the key's credentials that still have a live observation,
// evicting the rest.
func (o *observedSubscriptions) list(apiKeyID string, observer *usage.Observer) []observedSubscription {
	if o == nil || apiKeyID == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	var live []observedSubscription
	for _, entry := range o.byKey[apiKeyID] {
		if _, ok := observer.Snapshot(entry.key); ok {
			live = append(live, entry)
		}
	}
	if len(live) == 0 {
		delete(o.byKey, apiKeyID)
	} else {
		o.byKey[apiKeyID] = live
	}
	return live
}
