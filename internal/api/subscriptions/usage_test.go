package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	subscriptionsapi "weave-os/router/internal/api/subscriptions"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/server/middleware"
)

const (
	samRouterKey  = "rk_usage_sam"
	alexRouterKey = "rk_usage_alex"
	samClaudeTok  = "sk-ant-oat01-sam-claude-subscription"
	alexClaudeTok = "sk-ant-oat01-alex-claude-subscription"
)

// usageKeyRepo resolves each router key to its own personal subject.
type usageKeyRepo struct{ auth.APIKeyRepository }

func (usageKeyRepo) GetActiveByHashWithInstallation(_ context.Context, hash string) (*auth.APIKey, *auth.Installation, error) {
	installation := &auth.Installation{ID: "installation", ExternalID: "org-test"}
	switch hash {
	case auth.HashAPIKeySHA256(samRouterKey):
		return &auth.APIKey{ID: "key-sam", CredentialSubjectID: "subject-sam", InstallationID: "installation", Scope: auth.ScopeRouting}, installation, nil
	case auth.HashAPIKeySHA256(alexRouterKey):
		return &auth.APIKey{ID: "key-alex", CredentialSubjectID: "subject-alex", InstallationID: "installation", Scope: auth.ScopeRouting}, installation, nil
	}
	return nil, nil, auth.ErrInvalidToken
}

func (usageKeyRepo) MarkUsed(context.Context, string) (bool, error) { return false, nil }

type usageSubjects struct{}

func (usageSubjects) GetCredentialSubject(_ context.Context, subjectID, installationID string) (*auth.CredentialSubject, error) {
	if installationID != "installation" || (subjectID != "subject-sam" && subjectID != "subject-alex") {
		return nil, auth.ErrPersonalCredentialRequired
	}
	return &auth.CredentialSubject{ID: subjectID, ProjectionComplete: true, AccessEnabled: true}, nil
}

// ownedAccounts lists managed accounts strictly by the requesting subscriber.
type ownedAccounts struct {
	auth.SubscriptionAccountRepository
}

func (ownedAccounts) ListSubscriptionAccounts(_ context.Context, owner auth.SubscriptionOwner) ([]*auth.SubscriptionAccount, error) {
	switch owner.SubscriberID {
	case "subject-sam":
		return []*auth.SubscriptionAccount{{ID: "acct-sam-codex", SubscriberID: "subject-sam", Provider: auth.SubscriptionProviderCodex}}, nil
	case "subject-alex":
		return []*auth.SubscriptionAccount{{ID: "acct-alex-claude", SubscriberID: "subject-alex", Provider: auth.SubscriptionProviderClaude}}, nil
	}
	return nil, nil
}

type usageResponse struct {
	AsOf        time.Time `json:"as_of"`
	Credentials []struct {
		Provider      string     `json:"provider"`
		Source        string     `json:"source"`
		AccountID     string     `json:"account_id"`
		CredentialKey string     `json:"credential_key"`
		Observed      bool       `json:"observed"`
		ObservedAt    *time.Time `json:"observed_at"`
		Exhausted     bool       `json:"exhausted"`
		OverageInUse  bool       `json:"overage_in_use"`
		Windows       map[string]struct {
			Utilization   float64    `json:"utilization"`
			WindowMinutes int        `json:"window_minutes"`
			ResetAt       *time.Time `json:"reset_at"`
			Exhausted     bool       `json:"exhausted"`
		} `json:"windows"`
	} `json:"credentials"`
}

type usageFixture struct {
	engine   *gin.Engine
	observer *usage.Observer
	now      time.Time
}

func newUsageFixture(t *testing.T, observer bool) usageFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	authSvc := auth.NewService(installationRepo{}, usageKeyRepo{}, nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return now }).
		WithSubscriptionAccounts(ownedAccounts{}).WithCredentialSubjectLookup(usageSubjects{})
	proxySvc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	fixture := usageFixture{engine: gin.New(), now: now}
	if observer {
		fixture.observer = usage.NewObserver([]byte("salt"), 10*time.Minute, func() time.Time { return now })
		proxySvc.WithUsageObserver(fixture.observer)
	}
	subscriptionsapi.RegisterUsage(fixture.engine.Group("", middleware.WithAuth(authSvc, false)), authSvc, proxySvc)
	return fixture
}

func (f usageFixture) get(t *testing.T, routerKey, subscriptionToken string) (*httptest.ResponseRecorder, usageResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/subscriptions/usage", nil)
	if routerKey != "" {
		request.Header.Set(auth.RouterKeyHeader, routerKey)
	}
	if subscriptionToken != "" {
		request.Header.Set("Authorization", "Bearer "+subscriptionToken)
	}
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	var body usageResponse
	if recorder.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	}
	return recorder, body
}

func (f usageFixture) recordAnthropic(t *testing.T, key usage.CredentialKey, headers map[string]string) {
	t.Helper()
	h := http.Header{}
	for name, value := range headers {
		h.Set(name, value)
	}
	snapshot, ok := usage.ParseAnthropicUnifiedHeaders(h)
	require.True(t, ok)
	f.observer.Record(key, snapshot)
}

func TestUsageReturnsPresentedClaudeWindowsFromRecordedHeaders(t *testing.T) {
	f := newUsageFixture(t, true)
	f.recordAnthropic(t, f.observer.Key([]byte(samClaudeTok)), map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.25",
		"anthropic-ratelimit-unified-5h-reset":       "2026-10-06T14:30:00Z",
		"anthropic-ratelimit-unified-7d-utilization": "1.0",
		"anthropic-ratelimit-unified-7d-reset":       "2026-10-10T00:00:00Z",
	})

	recorder, body := f.get(t, samRouterKey, samClaudeTok)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, f.now, body.AsOf)
	require.Len(t, body.Credentials, 2, "the presented Claude token plus Sam's managed Codex account")
	claude := body.Credentials[0]
	assert.Equal(t, "claude", claude.Provider)
	assert.Equal(t, proxy.SubscriptionUsageSourcePresented, claude.Source)
	assert.Equal(t, string(f.observer.Key([]byte(samClaudeTok))), claude.CredentialKey)
	assert.True(t, claude.Observed)
	require.NotNil(t, claude.ObservedAt)
	assert.Equal(t, f.now, *claude.ObservedAt)
	assert.True(t, claude.Exhausted)
	assert.False(t, claude.OverageInUse)
	require.Len(t, claude.Windows, 2)
	assert.InDelta(t, 0.25, claude.Windows["primary"].Utilization, 1e-9)
	assert.Equal(t, 300, claude.Windows["primary"].WindowMinutes)
	require.NotNil(t, claude.Windows["primary"].ResetAt)
	assert.Equal(t, time.Date(2026, 10, 6, 14, 30, 0, 0, time.UTC), *claude.Windows["primary"].ResetAt)
	assert.False(t, claude.Windows["primary"].Exhausted)
	assert.InDelta(t, 1.0, claude.Windows["secondary"].Utilization, 1e-9)
	require.NotNil(t, claude.Windows["secondary"].ResetAt)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), *claude.Windows["secondary"].ResetAt)
	assert.True(t, claude.Windows["secondary"].Exhausted)

	managed := body.Credentials[1]
	assert.Equal(t, "codex", managed.Provider)
	assert.Equal(t, proxy.SubscriptionUsageSourceManaged, managed.Source)
	assert.Equal(t, "acct-sam-codex", managed.AccountID)
	assert.False(t, managed.Observed, "no reading yet for the managed account")
	assert.Empty(t, managed.Windows)

	assert.NotContains(t, recorder.Body.String(), samClaudeTok, "no raw subscription token in the response")
	assert.NotContains(t, recorder.Body.String(), samRouterKey, "no raw router key in the response")
}

func TestUsageReturnsOnlyCallersOwnCredentials(t *testing.T) {
	f := newUsageFixture(t, true)
	reading := map[string]string{"anthropic-ratelimit-unified-5h-utilization": "0.5", "anthropic-ratelimit-unified-5h-reset": "2026-10-06T14:00:00Z"}
	f.recordAnthropic(t, f.observer.Key([]byte(samClaudeTok)), reading)
	f.recordAnthropic(t, f.observer.Key([]byte(alexClaudeTok)), reading)
	f.recordAnthropic(t, f.observer.Key([]byte("subscription-account:acct-alex-claude")), reading)
	f.recordAnthropic(t, f.observer.Key([]byte("subscription-account:acct-sam-codex")), reading)

	recorder, body := f.get(t, alexRouterKey, alexClaudeTok)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	keys := map[string]bool{}
	for _, credential := range body.Credentials {
		keys[credential.CredentialKey] = true
		assert.NotEqual(t, "acct-sam-codex", credential.AccountID)
	}
	assert.Equal(t, map[string]bool{
		string(f.observer.Key([]byte(alexClaudeTok))):                           true,
		string(f.observer.Key([]byte("subscription-account:acct-alex-claude"))): true,
	}, keys)
	assert.NotContains(t, recorder.Body.String(), alexClaudeTok)

	// Without presenting a token, Alex sees only the managed account Alex owns.
	recorder, body = f.get(t, alexRouterKey, "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, body.Credentials, 1)
	assert.Equal(t, "acct-alex-claude", body.Credentials[0].AccountID)
	assert.True(t, body.Credentials[0].Observed)
}

func TestUsageRejectsUnauthenticatedCaller(t *testing.T) {
	f := newUsageFixture(t, true)
	recorder, _ := f.get(t, "", samClaudeTok)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), samClaudeTok)

	recorder, _ = f.get(t, "rk_unknown", samClaudeTok)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestUsageUnavailableWithoutObserver(t *testing.T) {
	f := newUsageFixture(t, false)
	recorder, _ := f.get(t, samRouterKey, samClaudeTok)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
}
