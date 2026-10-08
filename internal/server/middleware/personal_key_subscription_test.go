package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weave-os/router/internal/api/subscriptions"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type completeSubjectLookup struct{ subject auth.CredentialSubject }

func (l completeSubjectLookup) GetCredentialSubject(context.Context, string, string) (*auth.CredentialSubject, error) {
	subject := l.subject
	return &subject, nil
}

type enrolledSubscriptionRepository struct {
	failingSubscriptionAccountRepository
	accounts []*auth.SubscriptionAccount
	owners   []auth.SubscriptionOwner
}

func (r *enrolledSubscriptionRepository) ListSubscriptionCandidates(_ context.Context, owner auth.SubscriptionOwner) ([]*auth.SubscriptionAccount, error) {
	r.owners = append(r.owners, owner)
	return r.accounts, nil
}

func (r *enrolledSubscriptionRepository) ListSubscriptionAccounts(_ context.Context, owner auth.SubscriptionOwner) ([]*auth.SubscriptionAccount, error) {
	r.owners = append(r.owners, owner)
	return nil, nil
}

// A self-hosted personal key (complete, enabled projection) manages
// subscriptions, and its turns carry both enrolled providers and its subject
// as the owner, so a Claude or GPT pick can lease the matching account.
func TestSelfHostedPersonalKeyOwnsAndRoutesBothSubscriptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_self_hosted_personal"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	apiKey := &auth.APIKey{ID: "personal-key", InstallationID: "admin-installation", CredentialSubjectID: "operator-subject", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix, Scope: auth.ScopeRouting}
	installation := &auth.Installation{ID: apiKey.InstallationID, ExternalID: auth.AdminInstallationExternalID}
	apiKeys := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{hash: {apiKey: apiKey, installation: installation}}}
	repo := &enrolledSubscriptionRepository{accounts: []*auth.SubscriptionAccount{
		{ID: "claude-account", SubscriberID: "operator-subject", Provider: auth.SubscriptionProviderClaude, Enabled: true},
		{ID: "codex-account", SubscriberID: "operator-subject", Provider: auth.SubscriptionProviderCodex, Enabled: true},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, apiKeys, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now).
		WithCredentialSubjectLookup(completeSubjectLookup{subject: auth.CredentialSubject{ID: "operator-subject", ProjectionComplete: true, AccessEnabled: true, EnrollmentGeneration: 1}}).
		WithSubscriptionAccounts(repo)

	engine := gin.New()
	group := engine.Group("/v1", middleware.WithAuth(svc, false))
	subscriptions.Register(group, svc)
	var enrolled map[auth.SubscriptionProvider]struct{}
	var owner auth.SubscriptionOwner
	group.POST("/messages", func(c *gin.Context) {
		enrolled, _ = c.Request.Context().Value(proxy.ManagedSubscriptionProvidersContextKey{}).(map[auth.SubscriptionProvider]struct{})
		owner = middleware.SubscriptionOwnerFrom(c)
		c.Status(http.StatusOK)
	})

	list := httptest.NewRequest(http.MethodGet, "/v1/subscriptions/accounts", nil)
	list.Header.Set(middleware.RouterKeyHeader, routerToken)
	listed := httptest.NewRecorder()
	engine.ServeHTTP(listed, list)
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	assert.JSONEq(t, `[]`, listed.Body.String())

	turn := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	turn.Header.Set(middleware.RouterKeyHeader, routerToken)
	served := httptest.NewRecorder()
	engine.ServeHTTP(served, turn)
	require.Equal(t, http.StatusOK, served.Code)
	assert.Equal(t, map[auth.SubscriptionProvider]struct{}{auth.SubscriptionProviderClaude: {}, auth.SubscriptionProviderCodex: {}}, enrolled)
	assert.Equal(t, "operator-subject", owner.SubscriberID)
	assert.Equal(t, "operator-subject", repo.owners[len(repo.owners)-1].SubscriberID)
}
