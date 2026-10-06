package proxy

import (
	"errors"
	"net/http"
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
	// SubscriptionUsageSourceManaged is a router-managed account the caller's
	// verified subscription owner enrolled.
	SubscriptionUsageSourceManaged = "managed"
)

// ErrSubscriptionUsageUnavailable reports that no usage observer is wired, so
// an empty answer would falsely read as "no subscription is exhausted".
var ErrSubscriptionUsageUnavailable = errors.New("subscription usage observer is not configured")

// ManagedSubscriptionAccount names one managed account the caller owns.
type ManagedSubscriptionAccount struct {
	ID       string
	Provider auth.SubscriptionProvider
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

// SubscriptionUsageReading is the observed quota state of one caller-owned
// subscription credential. It carries only the salted credential key, never a
// token. Observed is false when no fresh reading exists for the credential.
type SubscriptionUsageReading struct {
	Provider       subscriptions.Provider
	Source         string
	AccountID      string
	CredentialKey  usage.CredentialKey
	Observed       bool
	ObservedAt     time.Time
	Exhausted      bool
	OverageInUse   bool
	UnifiedResetAt time.Time
	Windows        []SubscriptionUsageWindow
}

// SubscriptionUsage returns the observed rate-limit windows for exactly the
// credentials the caller owns: subscription tokens presented in headers (the
// same Claude Code / Codex CLI headers used on inference) and the managed
// accounts the caller's verified owner enrolled. Readings for any other
// credential are unreachable because the observer is addressed only by keys
// derived here. now is the observer clock used to judge exhaustion.
func (s *Service) SubscriptionUsage(headers http.Header, managed []ManagedSubscriptionAccount) (readings []SubscriptionUsageReading, now time.Time, err error) {
	if s.usageObserver == nil {
		return nil, time.Time{}, ErrSubscriptionUsageUnavailable
	}
	now = s.usageObserver.Now()
	read := func(provider subscriptions.Provider, source, accountID string, key usage.CredentialKey) {
		readings = append(readings, s.subscriptionUsageReading(provider, source, accountID, key, now))
	}
	if creds := ExtractClientCredentials(providers.ProviderAnthropic, headers); creds != nil && creds.OAuth && creds.Source == credSourceSubscription {
		read(subscriptions.ProviderClaude, SubscriptionUsageSourcePresented, "", s.usageObserver.Key(creds.APIKey))
	}
	if creds := ExtractClientCredentials(providers.ProviderOpenAI, headers); creds != nil && creds.OAuth && creds.Source == credSourceCodexSubscription {
		read(subscriptions.ProviderCodex, SubscriptionUsageSourcePresented, "", s.usageObserver.Key(creds.APIKey))
	}
	for _, account := range managed {
		if account.ID == "" {
			continue
		}
		read(subscriptions.Provider(account.Provider), SubscriptionUsageSourceManaged, account.ID, s.managedSubscriptionUsageKey(account.ID))
	}
	return readings, now, nil
}

func (s *Service) subscriptionUsageReading(provider subscriptions.Provider, source, accountID string, key usage.CredentialKey, now time.Time) SubscriptionUsageReading {
	reading := SubscriptionUsageReading{Provider: provider, Source: source, AccountID: accountID, CredentialKey: key}
	snapshot, observed := s.usageObserver.Snapshot(key)
	if !observed {
		return reading
	}
	reading.Observed = true
	reading.ObservedAt = snapshot.ObservedAt
	reading.Exhausted = snapshot.ExhaustedAsOf(now)
	reading.OverageInUse = snapshot.OverageInUse
	reading.UnifiedResetAt = snapshot.UnifiedResetAt
	for _, window := range [...]struct {
		name string
		usage.Window
	}{{"primary", snapshot.Primary}, {"secondary", snapshot.Secondary}} {
		if !window.Reported() {
			continue
		}
		reading.Windows = append(reading.Windows, SubscriptionUsageWindow{Name: window.name, Utilization: window.UsedPercent, WindowMinutes: window.WindowMinutes, ResetAt: window.ResetAt, Exhausted: window.Window.ExhaustedAsOf(now)})
	}
	return reading
}
