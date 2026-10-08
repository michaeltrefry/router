package subscription_verification_check_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/api/admin"
	subscriptionsapi "weave-os/router/internal/api/subscriptions"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/postgres"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/providers/anthropic"
	"weave-os/router/internal/providers/openai"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/server/middleware"
	"weave-os/router/internal/subscriptions"
)

type switchableRouter struct {
	mu       sync.Mutex
	decision router.Decision
}

func (r *switchableRouter) set(decision router.Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decision = decision
}

func (r *switchableRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.decision, nil
}

// A self-hosted personal key, issued the way `make personal-key` issues it,
// enrolls both subscriptions over the installer's API and then serves a Claude
// pick and a GPT pick from a Claude Code-style /v1/messages turn on those
// subscriptions, with no client OAuth credential on the request.
func TestVerificationSelfHostedPersonalKeyEnrollsAndServesBothSubscriptions(t *testing.T) {
	dsn := os.Getenv("ROUTER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("local Postgres integration requires ROUTER_TEST_DATABASE_URL")
	}
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1", "::1"}, parsed.Hostname())
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	repositories := postgres.NewRepository(pool, auth.NoOpEncryptor{})
	subjects := postgres.NewCredentialSubjectRepo(pool)
	authService := auth.NewService(repositories.Installations, repositories.APIKeys, repositories.ExternalAPIKeys, repositories.Users, auth.NoOpAPIKeyCache{}, nil, time.Now).
		WithEncryptor(auth.NoOpEncryptor{}).
		WithCredentialSubjectLookup(subjects).
		WithSubscriptionAccounts(repositories.SubscriptionAccounts).
		WithRoutingPolicies(repositories.RoutingPolicies, nil).
		WithCodexEnrollmentVerifier(verificationCodexEnrollment{}).
		WithPersonalKeyStore(subjects)

	existingAdmin, err := repositories.Installations.ListForExternalID(ctx, auth.AdminInstallationExternalID)
	require.NoError(t, err)
	installation, err := authService.EnsureAdminInstallation(ctx)
	require.NoError(t, err)
	email := "synthetic-operator-" + uuid.NewString()[:8] + "@example.invalid"
	var subjectID string
	t.Cleanup(func() {
		if subjectID != "" {
			_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_subscription_accounts WHERE subscriber_id=$1`, subjectID)
			_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_api_keys WHERE credential_subject_id=$1`, subjectID)
			_, _ = pool.Exec(ctx, `DELETE FROM router.credential_subject_identities WHERE subject_id=$1`, subjectID)
			_, _ = pool.Exec(ctx, `DELETE FROM router.credential_subject_installations WHERE subject_id=$1`, subjectID)
			_, _ = pool.Exec(ctx, `DELETE FROM router.credential_subjects WHERE id=$1`, subjectID)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_api_keys WHERE installation_id=$1 AND name='synthetic-shared'`, installation.ID)
		if len(existingAdmin) == 0 {
			_, _ = pool.Exec(ctx, `DELETE FROM router.model_router_installations WHERE id=$1`, installation.ID)
		}
	})

	issued, err := authService.IssueSelfHostedPersonalKey(ctx, auth.IssuePersonalKeyParams{Email: email})
	require.NoError(t, err)
	subjectID = issued.Key.CredentialSubjectID
	require.NotEmpty(t, subjectID)
	require.Equal(t, installation.ID, issued.Installation.ID)

	_, err = authService.IssueSelfHostedPersonalKey(ctx, auth.IssuePersonalKeyParams{Email: strings.ToUpper(email)})
	require.ErrorIs(t, err, auth.ErrPersonalKeyExists, "a re-run must not mint a second subject")

	sharedName := "synthetic-shared"
	_, sharedToken, err := authService.IssueAPIKey(ctx, installation.ID, &sharedName, nil)
	require.NoError(t, err)

	codexWorkspace := "synthetic-workspace-" + uuid.NewString()
	claudeAccount, claudeOrganization := uuid.NewString(), uuid.NewString()
	claudeExternalID := subscriptions.ClaudeExternalAccountID(claudeAccount, claudeOrganization)
	claudeTokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-claude-access", "refresh_token": "synthetic-claude-rotated", "expires_in": 3600, "account": map[string]any{"uuid": claudeAccount}, "organization": map[string]any{"uuid": claudeOrganization}})
	}))
	defer claudeTokens.Close()
	codexTokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": verificationCodexIDToken(t, codexWorkspace), "access_token": "synthetic-codex-access", "refresh_token": "synthetic-codex-rotated", "expires_in": 3600})
	}))
	defer codexTokens.Close()
	runtime := subscriptions.NewRuntime(authService, subscriptions.NewOAuthClient(http.DefaultClient, codexTokens.URL, claudeTokens.URL, time.Now), time.Now)

	var upstreamMu sync.Mutex
	var anthropicBearers, openAIBearers []string
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		anthropicBearers = append(anthropicBearers, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Api-Key"))
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_synthetic","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"claude subscription answer"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":4}}`)
	}))
	defer anthropicUpstream.Close()
	openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		openAIBearers = append(openAIBearers, r.Header.Get("Authorization"))
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"codex subscription answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"synthetic\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"codex subscription answer\"}]}],\"usage\":{\"input_tokens\":9,\"output_tokens\":4}}}\n\n")
	}))
	defer openAIUpstream.Close()
	openAIClient := openai.NewClient("synthetic-api-key", openAIUpstream.URL)
	openAIClient.SetCodexBaseURL(openAIUpstream.URL)
	routes := &switchableRouter{}
	proxyService := proxy.NewService(routes, map[string]providers.Client{
		providers.ProviderAnthropic: anthropic.NewClient("synthetic-anthropic-api-key", anthropicUpstream.URL),
		providers.ProviderOpenAI:    openAIClient,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-sonnet-4-6", nil).
		WithManagedSubscriptions(runtime).
		WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}, providers.ProviderOpenAI: {}})

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/validate", middleware.WithAuth(authService, false), admin.ValidateHandler)
	v1 := engine.Group("/v1", middleware.WithAuth(authService, false))
	subscriptionsapi.Register(v1, authService)
	var proxyErrors []error
	var winners []*proxy.ManagedSubscriptionUsage
	engine.POST("/v1/messages", middleware.WithAuth(authService, false), func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		proxyErrors = append(proxyErrors, proxyService.ProxyMessages(c.Request.Context(), body, c.Writer, c.Request))
		winners = append(winners, c.Request.Context().Value(proxy.ManagedSubscriptionUsageContextKey{}).(*proxy.ManagedSubscriptionUsage))
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	call := func(method, path, token string, body any) (int, string) {
		var reader io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			reader = bytes.NewReader(encoded)
		}
		request, err := http.NewRequest(method, server.URL+path, reader)
		require.NoError(t, err)
		request.Header.Set(auth.RouterKeyHeader, token)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		payload, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(payload)
	}

	status, _ := call(http.MethodGet, "/validate", issued.RawToken, nil)
	require.Equal(t, http.StatusOK, status)
	status, body := call(http.MethodGet, "/v1/subscriptions/accounts", issued.RawToken, nil)
	require.Equal(t, http.StatusOK, status, body)
	require.JSONEq(t, `[]`, body)
	status, body = call(http.MethodGet, "/v1/subscriptions/accounts", sharedToken, nil)
	require.Equal(t, http.StatusServiceUnavailable, status, "an installation-shared key still cannot own subscriptions")
	require.Contains(t, body, "subscription_owner_unavailable")

	status, body = call(http.MethodPost, "/v1/subscriptions/accounts", issued.RawToken, map[string]string{"provider": "claude", "external_account_id": claudeExternalID, "refresh_token": "synthetic-claude-refresh"})
	require.Equal(t, http.StatusCreated, status, body)
	status, body = call(http.MethodPost, "/v1/subscriptions/accounts", issued.RawToken, map[string]string{"provider": "codex", "external_account_id": codexWorkspace, "refresh_token": "synthetic-codex-refresh"})
	require.Equal(t, http.StatusCreated, status, body)

	turn := map[string]any{"model": "auto", "max_tokens": 256, "messages": []map[string]any{{"role": "user", "content": "synthetic worker turn"}}}
	routes.set(router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "test"})
	status, body = call(http.MethodPost, "/v1/messages", issued.RawToken, turn)
	require.NoError(t, proxyErrors[len(proxyErrors)-1])
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "claude subscription answer")
	require.Equal(t, []string{"Bearer synthetic-claude-access|"}, anthropicBearers, "the Claude pick is served on the enrolled Claude subscription, not the API key")
	require.True(t, winners[len(winners)-1].Served)
	require.Equal(t, auth.SubscriptionTierPersonal, winners[len(winners)-1].SubscriptionTier)

	routes.set(router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-sol", Reason: "test"})
	status, body = call(http.MethodPost, "/v1/messages", issued.RawToken, turn)
	require.NoError(t, proxyErrors[len(proxyErrors)-1])
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "codex subscription answer")
	require.Equal(t, []string{"Bearer synthetic-codex-access"}, openAIBearers, "a GPT pick for a Claude Code turn is served on the enrolled ChatGPT subscription")
	require.True(t, winners[len(winners)-1].Served)

	rotated, err := authService.IssueSelfHostedPersonalKey(ctx, auth.IssuePersonalKeyParams{Email: email, Rotate: true})
	require.NoError(t, err)
	require.True(t, rotated.Rotated)
	require.Equal(t, subjectID, rotated.Key.CredentialSubjectID)
	status, _ = call(http.MethodGet, "/validate", issued.RawToken, nil)
	require.Equal(t, http.StatusUnauthorized, status, "the replaced key is revoked")
	status, body = call(http.MethodGet, "/v1/subscriptions/accounts", rotated.RawToken, nil)
	require.Equal(t, http.StatusOK, status, body)
	var accounts []map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &accounts))
	require.Len(t, accounts, 2, "enrolled subscriptions survive rotation")

	// A key deleted from the dashboard leaves the subject keyless; rotation reissues for it.
	require.NoError(t, authService.DeleteAPIKey(ctx, installation.ID, rotated.Key.ID))
	reissued, err := authService.IssueSelfHostedPersonalKey(ctx, auth.IssuePersonalKeyParams{Email: email, Rotate: true})
	require.NoError(t, err)
	require.Equal(t, subjectID, reissued.Key.CredentialSubjectID)
	status, body = call(http.MethodGet, "/v1/subscriptions/accounts", reissued.RawToken, nil)
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &accounts))
	require.Len(t, accounts, 2)
}
