package subscriptions_test

import (
	"context"
	"encoding/json"
	"errors"
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
	patRouterKey  = "rk_usage_pat"
	orgRouterKey  = "rk_usage_org"
	orgClaudeTok  = "sk-ant-oat01-org-claude-subscription"
)

var (
	usageNow      = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	patCooldown   = usageNow.Add(90 * time.Minute)
	sharedDisplay = "Sam Shared Team Plan"
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
	case auth.HashAPIKeySHA256(patRouterKey):
		return &auth.APIKey{ID: "key-pat", CredentialSubjectID: "subject-pat", InstallationID: "installation", Scope: auth.ScopeRouting}, installation, nil
	case auth.HashAPIKeySHA256(orgRouterKey):
		return &auth.APIKey{ID: "key-org", InstallationID: "installation", Scope: auth.ScopeRouting}, installation, nil
	}
	return nil, nil, auth.ErrInvalidToken
}

func (usageKeyRepo) MarkUsed(context.Context, string) (bool, error) { return false, nil }

type usageSubjects struct{}

func (usageSubjects) GetCredentialSubject(_ context.Context, subjectID, installationID string) (*auth.CredentialSubject, error) {
	if installationID != "installation" || (subjectID != "subject-sam" && subjectID != "subject-alex" && subjectID != "subject-pat") {
		return nil, auth.ErrPersonalCredentialRequired
	}
	return &auth.CredentialSubject{ID: subjectID, ProjectionComplete: true, AccessEnabled: true}, nil
}

// candidateAccounts is serving admission: each subscriber's own accounts plus
// another member's shared account. It counts listings per request.
type candidateAccounts struct {
	auth.SubscriptionAccountRepository
	candidateCalls *int
	ownerCalls     *int
	fail           bool
}

func (a candidateAccounts) ListSubscriptionAccounts(context.Context, auth.SubscriptionOwner) ([]*auth.SubscriptionAccount, error) {
	*a.ownerCalls++
	return nil, nil
}

func (a candidateAccounts) ListSubscriptionCandidates(_ context.Context, owner auth.SubscriptionOwner) ([]*auth.SubscriptionAccount, error) {
	*a.candidateCalls++
	if a.fail {
		return nil, errors.New("database unavailable")
	}
	switch owner.SubscriberID {
	case "subject-sam":
		return []*auth.SubscriptionAccount{{ID: "acct-sam-codex", Tier: auth.SubscriptionTierPersonal, SubscriberID: "subject-sam", Provider: auth.SubscriptionProviderCodex, Enabled: true, State: auth.SubscriptionAccountStateActive}}, nil
	case "subject-alex":
		return []*auth.SubscriptionAccount{
			{ID: "acct-alex-claude", Tier: auth.SubscriptionTierPersonal, SubscriberID: "subject-alex", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateActive},
			{ID: "acct-sam-shared", Tier: auth.SubscriptionTierShared, SubscriberID: "subject-sam", DisplayName: sharedDisplay, ExternalAccountID: "sam-external", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateUnknown},
		}, nil
	case "subject-pat":
		return []*auth.SubscriptionAccount{
			{ID: "acct-pat-cooling", Tier: auth.SubscriptionTierPersonal, SubscriberID: "subject-pat", Provider: auth.SubscriptionProviderClaude, Enabled: true, State: auth.SubscriptionAccountStateCooldown, CooldownUntil: &patCooldown},
			{ID: "acct-pat-off", Tier: auth.SubscriptionTierPersonal, SubscriberID: "subject-pat", Provider: auth.SubscriptionProviderCodex, Enabled: false, State: auth.SubscriptionAccountStateDisabled},
		}, nil
	}
	return nil, nil
}

type usageCredential struct {
	Provider       string     `json:"provider"`
	Source         string     `json:"source"`
	AccountID      string     `json:"account_id"`
	CredentialKey  string     `json:"credential_key"`
	Routable       bool       `json:"routable"`
	State          string     `json:"state"`
	Enabled        *bool      `json:"enabled"`
	CooldownUntil  *time.Time `json:"cooldown_until"`
	Observed       bool       `json:"observed"`
	ObservedAt     *time.Time `json:"observed_at"`
	Exhausted      bool       `json:"exhausted"`
	ResumesAt      *time.Time `json:"resumes_at"`
	OverageInUse   bool       `json:"overage_in_use"`
	UnifiedResetAt *time.Time `json:"unified_reset_at"`
	Windows        map[string]struct {
		Utilization   float64    `json:"utilization"`
		WindowMinutes int        `json:"window_minutes"`
		ResetAt       *time.Time `json:"reset_at"`
		Exhausted     bool       `json:"exhausted"`
	} `json:"windows"`
}

type usageResponse struct {
	AsOf                time.Time         `json:"as_of"`
	AllExhausted        bool              `json:"all_exhausted"`
	ResumesAt           *time.Time        `json:"resumes_at"`
	KnownCredentials    int               `json:"known_credentials"`
	ObservedCredentials int               `json:"observed_credentials"`
	Credentials         []usageCredential `json:"credentials"`
}

type usageFixture struct {
	engine         *gin.Engine
	observer       *usage.Observer
	now            time.Time
	candidateCalls *int
	ownerCalls     *int
}

type usageFixtureOptions struct {
	noObserver     bool
	candidatesFail bool
}

func newUsageFixture(t *testing.T, observer bool) usageFixture {
	return newUsageFixtureWith(t, usageFixtureOptions{noObserver: !observer})
}

func newUsageFixtureWith(t *testing.T, options usageFixtureOptions) usageFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	now := usageNow
	fixture := usageFixture{engine: gin.New(), now: now, candidateCalls: new(int), ownerCalls: new(int)}
	accounts := candidateAccounts{candidateCalls: fixture.candidateCalls, ownerCalls: fixture.ownerCalls, fail: options.candidatesFail}
	authSvc := auth.NewService(installationRepo{}, usageKeyRepo{}, nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return now }).
		WithSubscriptionAccounts(accounts).WithCredentialSubjectLookup(usageSubjects{})
	proxySvc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	if !options.noObserver {
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
	assert.True(t, claude.Routable)
	require.NotNil(t, claude.ResumesAt)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), *claude.ResumesAt, "resumes when the spent weekly window resets")
	assert.Empty(t, claude.State, "a pass-through token carries no managed state")
	assert.Nil(t, claude.Enabled)

	managed := body.Credentials[1]
	assert.Equal(t, "codex", managed.Provider)
	assert.Equal(t, proxy.SubscriptionUsageSourceManaged, managed.Source)
	assert.Equal(t, "acct-sam-codex", managed.AccountID)
	assert.False(t, managed.Observed, "no reading yet for the managed account")
	assert.Empty(t, managed.Windows)
	assert.True(t, managed.Routable)
	assert.False(t, managed.Exhausted)
	assert.Equal(t, "active", managed.State)
	require.NotNil(t, managed.Enabled)
	assert.True(t, *managed.Enabled)

	assert.False(t, body.AllExhausted, "the unobserved managed account is still routable capacity")
	assert.Equal(t, 2, body.KnownCredentials)
	assert.Equal(t, 1, body.ObservedCredentials, "observed:false is not headroom; the counts expose it")
	require.NotNil(t, body.ResumesAt)
	assert.Equal(t, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), *body.ResumesAt)
	assert.Equal(t, 1, *f.candidateCalls, "the poll reuses the serving-admission listing WithAuth ran")
	assert.Zero(t, *f.ownerCalls, "no second account listing per poll")

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
		string(f.observer.Key([]byte("subscription-account:acct-sam-shared"))):  true,
	}, keys)
	assert.NotContains(t, recorder.Body.String(), alexClaudeTok)

	// Without presenting a token, Alex sees the managed account Alex owns and
	// Sam's shared account Alex can be dispatched onto, the latter state-only.
	recorder, body = f.get(t, alexRouterKey, "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, body.Credentials, 2)
	assert.Equal(t, "acct-alex-claude", body.Credentials[0].AccountID)
	assert.True(t, body.Credentials[0].Observed)
	shared := body.Credentials[1]
	assert.Equal(t, proxy.SubscriptionUsageSourceShared, shared.Source)
	assert.Empty(t, shared.AccountID, "another owner's account id is never returned")
	assert.Equal(t, "unknown", shared.State)
	assert.True(t, shared.Routable)
	for _, foreign := range []string{"acct-sam-shared", sharedDisplay, "subject-sam", "sam-external"} {
		assert.NotContains(t, recorder.Body.String(), foreign)
	}
	assert.Equal(t, 2, body.KnownCredentials)
}

// Managed accounts carry durable health: a future cooldown is exhaustion even
// with no observation, and a disabled account is reported but not routable.
func TestUsageReportsManagedCooldownAndNonRoutableAccounts(t *testing.T) {
	f := newUsageFixture(t, true)
	recorder, body := f.get(t, patRouterKey, "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, body.Credentials, 2)
	cooling, off := body.Credentials[0], body.Credentials[1]
	assert.Equal(t, "acct-pat-cooling", cooling.AccountID)
	assert.False(t, cooling.Observed)
	assert.True(t, cooling.Exhausted, "a future cooldown_until blocks routing")
	assert.True(t, cooling.Routable)
	assert.Equal(t, "cooldown", cooling.State)
	require.NotNil(t, cooling.CooldownUntil)
	assert.Equal(t, patCooldown, *cooling.CooldownUntil)
	require.NotNil(t, cooling.ResumesAt)
	assert.Equal(t, patCooldown, *cooling.ResumesAt)

	assert.Equal(t, "acct-pat-off", off.AccountID)
	assert.False(t, off.Routable, "a disabled account needs a human and never recovers on its own")
	assert.Equal(t, "disabled", off.State)
	require.NotNil(t, off.Enabled)
	assert.False(t, *off.Enabled)

	assert.True(t, body.AllExhausted, "the only routable account is cooling down")
	assert.Equal(t, 1, body.KnownCredentials, "non-routable accounts are excluded")
	assert.Zero(t, body.ObservedCredentials)
	require.NotNil(t, body.ResumesAt)
	assert.Equal(t, patCooldown, *body.ResumesAt)
}

// Paid overage is billable, so the router no longer treats it as free capacity.
func TestUsageReportsPaidOverageAsExhausted(t *testing.T) {
	f := newUsageFixture(t, true)
	f.recordAnthropic(t, f.observer.Key([]byte(orgClaudeTok)), map[string]string{
		"anthropic-ratelimit-unified-representative-claim": "overage",
		"anthropic-ratelimit-unified-overage-in-use":       "true",
		"anthropic-ratelimit-unified-reset":                "2026-10-06T16:00:00Z",
		"anthropic-ratelimit-unified-5h-utilization":       "0.3",
	})
	recorder, body := f.get(t, orgRouterKey, orgClaudeTok)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Len(t, body.Credentials, 1)
	claude := body.Credentials[0]
	assert.True(t, claude.OverageInUse)
	assert.True(t, claude.Exhausted, "paid overage reads as exhausted")
	assert.False(t, claude.Windows["primary"].Exhausted)
	resume := time.Date(2026, 10, 6, 16, 0, 0, 0, time.UTC)
	require.NotNil(t, claude.ResumesAt)
	assert.Equal(t, resume, *claude.ResumesAt)
	assert.True(t, body.AllExhausted)
	require.NotNil(t, body.ResumesAt)
	assert.Equal(t, resume, *body.ResumesAt)
}

// A caller this process knows nothing about is neither exhausted nor headroom.
func TestUsageWithNothingKnownIsNotExhausted(t *testing.T) {
	f := newUsageFixture(t, true)
	recorder, body := f.get(t, orgRouterKey, "")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, body.Credentials)
	assert.False(t, body.AllExhausted)
	assert.Nil(t, body.ResumesAt)
	assert.Zero(t, body.KnownCredentials)
	assert.Contains(t, recorder.Body.String(), `"known_credentials":0`)
}

func TestUsageUnavailableWhenAdmissionListingFails(t *testing.T) {
	f := newUsageFixtureWith(t, usageFixtureOptions{candidatesFail: true})
	recorder, _ := f.get(t, samRouterKey, "")
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "subscription_accounts_unavailable")
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
