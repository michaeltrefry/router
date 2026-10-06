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

// UsageReader reads observed subscription quota windows for the credentials a
// caller presents or owns. *proxy.Service implements it.
type UsageReader interface {
	SubscriptionUsage(headers http.Header, managed []proxy.ManagedSubscriptionAccount) ([]proxy.SubscriptionUsageReading, time.Time, error)
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
	Observed       bool                           `json:"observed"`
	ObservedAt     *time.Time                     `json:"observed_at,omitempty"`
	Exhausted      bool                           `json:"exhausted"`
	OverageInUse   bool                           `json:"overage_in_use"`
	UnifiedResetAt *time.Time                     `json:"unified_reset_at,omitempty"`
	Windows        map[string]usageWindowResponse `json:"windows"`
}

type usageResponse struct {
	AsOf        time.Time                 `json:"as_of"`
	Credentials []usageCredentialResponse `json:"credentials"`
}

// RegisterUsage mounts the read-only subscription usage endpoint on an
// authenticated group. It does not depend on managed accounts being enabled:
// presented Claude Code / Codex CLI tokens are readable either way.
func RegisterUsage(group *gin.RouterGroup, authSvc *auth.Service, reader UsageReader) {
	group.GET("/v1/subscriptions/usage", usageHandler(authSvc, reader))
}

// usageHandler reports observed rate-limit windows for exactly the caller's
// credentials: subscription tokens presented on this request (proof of
// possession, same headers as inference) and the managed accounts enrolled by
// the caller's verified subscription owner. Credentials are identified only by
// their salted hash; no token is ever echoed.
func usageHandler(authSvc *auth.Service, reader UsageReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.APIKeyFrom(c)
		if key == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "router_key_required"})
			return
		}
		managed, err := ownedManagedAccounts(c, authSvc, key)
		if err != nil {
			observability.FromGin(c).Error("Failed to list managed subscription accounts for usage", "err", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "subscription_accounts_unavailable"})
			return
		}
		readings, now, err := reader.SubscriptionUsage(c.Request.Header, managed)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "subscription_usage_unavailable"})
			return
		}
		response := usageResponse{AsOf: now.UTC(), Credentials: make([]usageCredentialResponse, 0, len(readings))}
		for _, reading := range readings {
			response.Credentials = append(response.Credentials, usageCredential(reading))
		}
		c.JSON(http.StatusOK, response)
	}
}

// ownedManagedAccounts lists managed accounts only for a personal key whose
// subject is still verified; an organization key owns none.
func ownedManagedAccounts(c *gin.Context, authSvc *auth.Service, key *auth.APIKey) ([]proxy.ManagedSubscriptionAccount, error) {
	if !authSvc.SubscriptionAccountsEnabled() {
		return nil, nil
	}
	owner, err := authSvc.SubscriptionOwnerForRequestUncached(c.Request.Context(), key)
	if errors.Is(err, auth.ErrPersonalCredentialRequired) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	accounts, err := authSvc.ListSubscriptionAccounts(c.Request.Context(), owner)
	if err != nil {
		return nil, err
	}
	managed := make([]proxy.ManagedSubscriptionAccount, 0, len(accounts))
	for _, account := range accounts {
		managed = append(managed, proxy.ManagedSubscriptionAccount{ID: account.ID, Provider: account.Provider})
	}
	return managed, nil
}

func usageCredential(reading proxy.SubscriptionUsageReading) usageCredentialResponse {
	out := usageCredentialResponse{
		Provider: string(reading.Provider), Source: reading.Source, AccountID: reading.AccountID,
		CredentialKey: string(reading.CredentialKey), Observed: reading.Observed,
		ObservedAt: timePtr(reading.ObservedAt), Exhausted: reading.Exhausted, OverageInUse: reading.OverageInUse,
		UnifiedResetAt: timePtr(reading.UnifiedResetAt), Windows: make(map[string]usageWindowResponse, len(reading.Windows)),
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
