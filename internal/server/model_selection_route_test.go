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

func (r selectionInstallationRepo) UpdatePreferredModels(_ context.Context, _, _ string, models []string) error {
	r.installation.PreferredModels = append([]string{}, models...)
	return nil
}

// The installer edits model selection with the installation's router key, so
// those routes accept it in self-hosted mode while credential management stays
// cookie-only.
func TestModelSelectionRoutesAcceptRouterKeyInSelfHostedMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installation := &auth.Installation{ID: uuid.NewString(), ExternalID: "ext-selection"}
	keys := readKeyRepo{installation: installation, scopes: map[string]auth.APIKeyScope{"rk_selection": auth.ScopeRouting}}
	authSvc := auth.NewService(selectionInstallationRepo{installation: installation}, keys, nil, nil, auth.NoOpAPIKeyCache{}, nil, time.Now)
	proxySvc := proxy.NewService(nil, nil, nil, false, nil, nil, false, "", "", nil)
	engine := gin.New()
	server.Register(engine, authSvc, proxySvc, fakeDeployedModelsSource{}, nil, server.DeploymentModeSelfHosted, nil, nil, nil, nil)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(auth.RouterKeyHeader, "rk_selection")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}

	rec := call(http.MethodGet, "/admin/v1/models", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"model":"claude-opus-5"`)

	rec = call(http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["claude-opus-5"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"claude-opus-5"}, installation.PreferredModels)

	rec = call(http.MethodGet, "/admin/v1/keys", "")
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"error":"admin_`, "a router key must not reach credential management")
}
