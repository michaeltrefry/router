package subscriptions

import (
	"errors"
	"net/http"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// UsageReader reads subscription quota readings for the credentials a caller
// presents, was observed using, or can be dispatched onto. *proxy.Service
// implements it.
type UsageReader interface {
	SubscriptionUsage(headers http.Header, apiKeyID string, managed []proxy.ManagedSubscriptionAccount) ([]proxy.SubscriptionUsageReading, time.Time, error)
}

type usageWindowResponse struct {
	Utilization   float64    `json:"utilization"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	Exhausted     bool       `json:"exhausted"`
}

type usageCredentialResponse struct {
	Provider       string                         `json:"provider"`
	Source         string                         `json:"source"`
	AccountID      string                         `json:"account_id,omitempty"`
	CredentialKey  string                         `json:"credential_key"`
	Routable       bool                           `json:"routable"`
	State          string                         `json:"state,omitempty"`
	Enabled        *bool                          `json:"enabled,omitempty"`
	CooldownUntil  *time.Time                     `json:"cooldown_until,omitempty"`
	Observed       bool                           `json:"observed"`
	ObservedAt     *time.Time                     `json:"observed_at,omitempty"`
	Exhausted      bool                           `json:"exhausted"`
	ResumesAt      *time.Time                     `json:"resumes_at,omitempty"`
	OverageInUse   bool                           `json:"overage_in_use"`
	UnifiedResetAt *time.Time                     `json:"unified_reset_at,omitempty"`
	Windows        map[string]usageWindowResponse `json:"windows"`
}

type usageResponse struct {
	AsOf                time.Time                 `json:"as_of"`
	AllExhausted        bool                      `json:"all_exhausted"`
	ResumesAt           *time.Time                `json:"resumes_at,omitempty"`
	KnownCredentials    int                       `json:"known_credentials"`
	ObservedCredentials int                       `json:"observed_credentials"`
	Credentials         []usageCredentialResponse `json:"credentials"`
}

// RegisterUsage mounts the read-only subscription usage endpoint on an
// authenticated group. It does not depend on managed accounts being enabled:
// pass-through Claude Code / Codex CLI tokens are readable either way.
func RegisterUsage(group *gin.RouterGroup, authSvc *auth.Service, reader UsageReader) {
	group.GET("/v1/subscriptions/usage", usageHandler(authSvc, reader))
}

// usageHandler reports quota state for exactly the credentials the caller can
// be routed onto: subscription tokens presented on this request, pass-through
// tokens this process observed under the caller's router key, and the managed
// accounts serving admission grants the caller (own and shared). Credentials
// are identified only by their salted hash; no token is ever echoed.
func usageHandler(authSvc *auth.Service, reader UsageReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.APIKeyFrom(c)
		if key == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "router_key_required"})
			return
		}
		managed, err := admittedManagedAccounts(c, authSvc)
		if err != nil {
			observability.FromGin(c).Error("Failed to list managed subscription accounts for usage", "err", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "subscription_accounts_unavailable"})
			return
		}
		readings, now, err := reader.SubscriptionUsage(c.Request.Header, key.ID, managed)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "subscription_usage_unavailable"})
			return
		}
		summary := proxy.SummarizeSubscriptionUsage(readings)
		response := usageResponse{
			AsOf: now.UTC(), AllExhausted: summary.AllExhausted, ResumesAt: timePtr(summary.ResumesAt),
			KnownCredentials: summary.KnownCredentials, ObservedCredentials: summary.ObservedCredentials,
			Credentials: make([]usageCredentialResponse, 0, len(readings)),
		}
		for _, reading := range readings {
			response.Credentials = append(response.Credentials, usageCredential(reading))
		}
		c.JSON(http.StatusOK, response)
	}
}

// errSubscriptionCandidatesNotResolved means WithAuth did not run serving
// admission for this request although managed accounts are on; reporting no
// accounts would hide capacity, so the read fails closed.
var errSubscriptionCandidatesNotResolved = errors.New("subscription candidates were not resolved for this request")

// admittedManagedAccounts reuses the serving-admission listing WithAuth already
// ran (no extra round trip). That SQL reads live subject membership, so a
// revoked or disabled subject loses its accounts on the same request; no
// separate uncached subject check is needed. Another member's shared account
// keeps only its id for the observer key; the response omits it.
func admittedManagedAccounts(c *gin.Context, authSvc *auth.Service) ([]proxy.ManagedSubscriptionAccount, error) {
	if !authSvc.SubscriptionAccountsEnabled() {
		return nil, nil
	}
	candidates, ok := middleware.SubscriptionCandidatesFrom(c)
	if !ok {
		return nil, errSubscriptionCandidatesNotResolved
	}
	if candidates.Err != nil {
		return nil, candidates.Err
	}
	managed := make([]proxy.ManagedSubscriptionAccount, 0, len(candidates.Accounts))
	for _, account := range candidates.Accounts {
		entry := proxy.ManagedSubscriptionAccount{
			ID: account.ID, Provider: account.Provider, Shared: account.Tier == auth.SubscriptionTierShared,
			Enabled: account.Enabled, State: account.State,
		}
		if account.CooldownUntil != nil {
			entry.CooldownUntil = *account.CooldownUntil
		}
		managed = append(managed, entry)
	}
	return managed, nil
}

func usageCredential(reading proxy.SubscriptionUsageReading) usageCredentialResponse {
	out := usageCredentialResponse{
		Provider: string(reading.Provider), Source: reading.Source, AccountID: reading.AccountID,
		CredentialKey: string(reading.CredentialKey), Routable: reading.Routable, Observed: reading.Observed,
		ObservedAt: timePtr(reading.ObservedAt), Exhausted: reading.Exhausted, ResumesAt: timePtr(reading.ResumesAt),
		OverageInUse: reading.OverageInUse, UnifiedResetAt: timePtr(reading.UnifiedResetAt),
		Windows: make(map[string]usageWindowResponse, len(reading.Windows)),
	}
	if reading.Source == proxy.SubscriptionUsageSourceManaged || reading.Source == proxy.SubscriptionUsageSourceShared {
		enabled := reading.Enabled
		out.State, out.Enabled, out.CooldownUntil = string(reading.State), &enabled, timePtr(reading.CooldownUntil)
	}
	for _, window := range reading.Windows {
		out.Windows[window.Name] = usageWindowResponse{Utilization: window.Utilization, WindowMinutes: window.WindowMinutes, ResetAt: timePtr(window.ResetAt), Exhausted: window.Exhausted}
	}
	return out
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
