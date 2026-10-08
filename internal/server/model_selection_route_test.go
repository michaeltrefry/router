package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/server"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type selectionInstallationRepo struct {
	auth.InstallationRepository
	installation *auth.Installation
}

func (selectionInstallationRepo) MarkFirstRequestServed(context.Context, string) error { return nil }

func (r selectionInstallationRepo) ListForExternalID(_ context.Context, externalID string) ([]*auth.Installation, error) {
	if externalID != r.installation.ExternalID {
		return nil, nil
	}
	return []*auth.Installation{r.installation}, nil
}

func (r selectionInstallationRepo) UpdatePreferredModels(_ context.Context, _, _ string, models []string) error {
	r.installation.PreferredModels = append([]string{}, models...)
	return nil
}

const (
	selectionSharedKey   = "rk_selection_shared"
	selectionPersonalKey = "rk_selection_personal"
	selectionAnalyticKey = "ra_selection_analytics"
	selectionAdminPass   = "selection-admin-password"
)

type selectionHarness struct {
	engine       *gin.Engine
	installation *auth.Installation
	cookie       string
}

func newSelectionHarness(t *testing.T, mode server.DeploymentMode) selectionHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	installation := &auth.Installation{ID: uuid.NewString(), ExternalID: auth.AdminInstallationExternalID}
	keys := readKeyRepo{
		installation: installation,
		scopes: map[string]auth.APIKeyScope{
			selectionSharedKey:   auth.ScopeRouting,
			selectionPersonalKey: auth.ScopeRouting,
			selectionAnalyticKey: auth.ScopeAnalyticsRead,
		},
		subjectIDs: map[string]string{selectionPersonalKey: "subject-selection"},
	}
	authSvc := auth.NewService(selectionInstallationRepo{installation: installation}, keys, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now).
		WithAdminPassword(selectionAdminPass)
	cookie, _, err := authSvc.IssueAdminSession()
	require.NoError(t, err)
	proxySvc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	engine := gin.New()
	server.Register(engine, authSvc, proxySvc, fakeDeployedModelsSource{}, nil, mode, nil, nil, nil, nil)
	return selectionHarness{engine: engine, installation: installation, cookie: cookie}
}

func (h selectionHarness) withKey(method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(auth.RouterKeyHeader, key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	return rec
}

func (h selectionHarness) withCookie(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AdminSessionCookieName, Value: h.cookie})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	return rec
}

var selectionReads = []string{
	"/admin/v1/models",
	"/admin/v1/providers",
	"/admin/v1/excluded-models",
	"/admin/v1/excluded-providers",
	"/admin/v1/preferred-models",
}

var selectionWrites = []struct{ method, path, body string }{
	{http.MethodPut, "/admin/v1/excluded-models", `{"excluded":[]}`},
	{http.MethodPost, "/admin/v1/excluded-models", `{"model":"claude-opus-5"}`},
	{http.MethodPost, "/admin/v1/excluded-models/remove", `{"model":"claude-opus-5"}`},
	{http.MethodPut, "/admin/v1/excluded-providers", `{"excluded":[]}`},
	{http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"anthropic"}`},
	{http.MethodPost, "/admin/v1/excluded-providers/remove", `{"provider":"anthropic"}`},
	{http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["claude-opus-5"]}`},
	{http.MethodPost, "/admin/v1/preferred-models", `{"model":"claude-opus-5"}`},
	{http.MethodPost, "/admin/v1/preferred-models/remove", `{"model":"claude-opus-5"}`},
}

// Router keys, shared or personal, may read the model selection; every write
// is dashboard-only and the refusal names the dashboard page.
func TestModelSelectionRouterKeysReadButCannotWrite(t *testing.T) {
	h := newSelectionHarness(t, server.DeploymentModeSelfHosted)
	for _, key := range []string{selectionSharedKey, selectionPersonalKey} {
		for _, path := range selectionReads {
			rec := h.withKey(http.MethodGet, path, "", key)
			assert.Equal(t, http.StatusOK, rec.Code, "%s GET %s: %s", key, path, rec.Body.String())
		}
		for _, w := range selectionWrites {
			rec := h.withKey(w.method, w.path, w.body, key)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s %s: %s", key, w.method, w.path, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "http://example.com/ui/settings/models", "%s %s %s", key, w.method, w.path)
		}
	}
	assert.Empty(t, h.installation.PreferredModels, "no router-key write may land")
}

func TestModelSelectionRejectsOtherCredentials(t *testing.T) {
	h := newSelectionHarness(t, server.DeploymentModeSelfHosted)
	for _, key := range []string{selectionAnalyticKey, "", "rk_revoked_or_unknown"} {
		rec := h.withKey(http.MethodGet, "/admin/v1/models", "", key)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "GET with %q: %s", key, rec.Body.String())
		rec = h.withKey(http.MethodPut, "/admin/v1/excluded-models", `{"excluded":[]}`, key)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "PUT with %q: %s", key, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "/ui/settings/models", "only a valid router key is pointed at the dashboard")
	}
}

func TestModelSelectionDashboardCookieCanWrite(t *testing.T) {
	h := newSelectionHarness(t, server.DeploymentModeSelfHosted)
	rec := h.withCookie(http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["claude-opus-5"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"claude-opus-5"}, h.installation.PreferredModels)
}

func TestCredentialManagementStaysCookieOnly(t *testing.T) {
	h := newSelectionHarness(t, server.DeploymentModeSelfHosted)
	for _, path := range []string{"/admin/v1/keys", "/admin/v1/provider-keys"} {
		rec := h.withKey(http.MethodGet, path, "", selectionSharedKey)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, path)
		assert.JSONEq(t, `{"error":"admin_session_required"}`, rec.Body.String(), path)
	}
}

func TestManagedModeMountsNoModelSelection(t *testing.T) {
	h := newSelectionHarness(t, server.DeploymentModeManaged)
	for _, path := range selectionReads {
		assert.Equal(t, http.StatusNotFound, h.withKey(http.MethodGet, path, "", selectionSharedKey).Code, path)
	}
	for _, w := range selectionWrites {
		assert.Equal(t, http.StatusNotFound, h.withKey(w.method, w.path, w.body, selectionSharedKey).Code, w.method+" "+w.path)
	}
}
