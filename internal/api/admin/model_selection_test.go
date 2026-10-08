package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/api/admin"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modelSelectionToken = "rk_model_selection"

// modelSelectionStore backs both repositories with one installation, so a
// write through the installation repo is what the next request authenticates
// into — the same round trip the installer makes.
type modelSelectionStore struct {
	installation auth.Installation
	writes       int
}

type modelSelectionKeyRepo struct {
	auth.APIKeyRepository
	store *modelSelectionStore
}

func (r modelSelectionKeyRepo) GetActiveByHashWithInstallation(_ context.Context, hash string) (*auth.APIKey, *auth.Installation, error) {
	if hash != auth.HashAPIKeySHA256(modelSelectionToken) {
		return nil, nil, sql.ErrNoRows
	}
	inst := r.store.installation
	return &auth.APIKey{ID: "key-1", InstallationID: inst.ID, Scope: auth.ScopeRouting}, &inst, nil
}

func (modelSelectionKeyRepo) MarkUsed(context.Context, string) (bool, error) { return false, nil }

type modelSelectionInstallationRepo struct {
	auth.InstallationRepository
	store *modelSelectionStore
}

func (modelSelectionInstallationRepo) MarkFirstRequestServed(context.Context, string) error {
	return nil
}

func (r modelSelectionInstallationRepo) UpdateExcludedModels(_ context.Context, _, _ string, models []string) error {
	r.store.writes++
	r.store.installation.ExcludedModels = append([]string{}, models...)
	return nil
}

func (r modelSelectionInstallationRepo) UpdateExcludedProviders(_ context.Context, _, _ string, names []string) error {
	r.store.writes++
	r.store.installation.ExcludedProviders = append([]string{}, names...)
	return nil
}

func (r modelSelectionInstallationRepo) UpdatePreferredModels(_ context.Context, _, _ string, models []string) error {
	r.store.writes++
	r.store.installation.PreferredModels = append([]string{}, models...)
	return nil
}

type fakeRoutable map[string]struct{}

func (f fakeRoutable) RoutableModels() map[string]struct{} { return f }

type fakeDeployed []cluster.DeployedEntry

func (f fakeDeployed) DefaultDeployedModels() []cluster.DeployedEntry { return f }

type fakeExclusionOverride struct{ models, providers []string }

func (f fakeExclusionOverride) HasExcludedModelsOverride() bool     { return f.models != nil }
func (f fakeExclusionOverride) ExcludedModelsOverride() []string    { return f.models }
func (f fakeExclusionOverride) HasExcludedProvidersOverride() bool  { return f.providers != nil }
func (f fakeExclusionOverride) ExcludedProvidersOverride() []string { return f.providers }

type modelSelectionFixture struct {
	engine     *gin.Engine
	store      *modelSelectionStore
	localModel string
	localProv  string
}

func newModelSelectionFixture(t *testing.T, override fakeExclusionOverride) modelSelectionFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	const localID = "test-local-selection"
	localProvider := registerTestLocalModel(t, localID)
	store := &modelSelectionStore{installation: auth.Installation{ID: "inst-1", ExternalID: "ext-1"}}
	authSvc := auth.NewService(
		modelSelectionInstallationRepo{store: store},
		modelSelectionKeyRepo{store: store},
		nil, nil, auth.NoOpAPIKeyCache{}, nil,
		func() time.Time { return time.Unix(0, 0) },
	)
	routable := fakeRoutable{"claude-opus-5": {}, "claude-opus-5-5": {}, "gpt-5.5": {}, localID: {}}
	deployed := fakeDeployed{
		{Model: "claude-opus-5", Provider: providers.ProviderAnthropic},
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI},
	}

	engine := gin.New()
	g := engine.Group("/admin/v1", middleware.WithAdminOrAuth(authSvc, false))
	g.GET("/models", admin.GetModelsHandler(authSvc, routable, override))
	g.GET("/excluded-models", admin.GetExcludedModelsHandler(authSvc, deployed, override))
	g.POST("/excluded-models", admin.AddExcludedModelHandler(authSvc, override))
	g.POST("/excluded-models/remove", admin.RemoveExcludedModelHandler(authSvc, override))
	g.GET("/preferred-models", admin.GetPreferredModelsHandler(authSvc))
	g.PUT("/preferred-models", admin.UpdatePreferredModelsHandler(authSvc, routable))
	g.POST("/preferred-models", admin.AddPreferredModelHandler(authSvc, routable))
	g.POST("/preferred-models/remove", admin.RemovePreferredModelHandler(authSvc, routable))
	g.GET("/providers", admin.GetProvidersHandler(authSvc, deployed, routable, override))
	g.GET("/excluded-providers", admin.GetExcludedProvidersHandler(authSvc, deployed, routable, override))
	g.POST("/excluded-providers", admin.AddExcludedProviderHandler(authSvc, deployed, routable, override))
	g.POST("/excluded-providers/remove", admin.RemoveExcludedProviderHandler(authSvc, deployed, routable, override))
	return modelSelectionFixture{engine: engine, store: store, localModel: localID, localProv: localProvider}
}

// do sends the request the way the installer does: the router key in
// X-Weave-Router-Key, never in an Authorization header.
func (f modelSelectionFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(auth.RouterKeyHeader, modelSelectionToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.engine.ServeHTTP(rec, req)
	return rec
}

type modelRow struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Local    bool   `json:"local"`
}

func TestGetModels_ListsRoutableUniverseWithEnabledState(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	f.store.installation.ExcludedModels = []string{"claude-opus-5"}

	rec := f.do(t, http.MethodGet, "/admin/v1/models", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []modelRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Equal(t, []modelRow{
		{Model: "claude-opus-5", Provider: providers.ProviderAnthropic, Enabled: false},
		{Model: "claude-opus-5-5", Provider: providers.ProviderAnthropic, Enabled: true},
		{Model: f.localModel, Provider: f.localProv, Enabled: true, Local: true},
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI, Enabled: true},
	}, rows, "the routable universe, mapping target and local model included, sorted by provider then model")
}

func TestGetModels_RejectsMissingKey(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	rec := httptest.NewRecorder()
	f.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/models", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExcludedModelItems_EditTheDashboardList(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})

	for range 2 {
		rec := f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"`+f.localModel+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.Equal(t, []string{f.localModel}, f.store.installation.ExcludedModels, "adding twice is idempotent")

	rec := f.do(t, http.MethodGet, "/admin/v1/excluded-models", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var dashboard excludedModelsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dashboard))
	assert.Equal(t, []string{f.localModel}, dashboard.Excluded, "the dashboard reads the list the CLI wrote")

	rec = f.do(t, http.MethodGet, "/admin/v1/models", "")
	var rows []modelRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	for _, r := range rows {
		assert.Equal(t, r.Model != f.localModel, r.Enabled, r.Model)
	}

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models/remove", `{"model":"`+f.localModel+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, f.store.installation.ExcludedModels)

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"not-a-model"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, f.store.installation.ExcludedModels)
}

func TestExcludedModelItems_EnvOverrideForbidsWrites(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{models: []string{"gpt-5.5"}})

	rec := f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"claude-opus-5"}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Zero(t, f.store.writes)
	rec = f.do(t, http.MethodGet, "/admin/v1/models", "")
	var rows []modelRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	for _, r := range rows {
		assert.Equal(t, r.Model != "gpt-5.5", r.Enabled, "the env override decides enabled state: %s", r.Model)
	}
}

func TestPreferredModels_ReplaceAddRemove(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})

	rec := f.do(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["claude-opus-5-5","gpt-5.5","claude-opus-5-5"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"preferred":["claude-opus-5-5","gpt-5.5"]}`, rec.Body.String())
	assert.Equal(t, []string{"claude-opus-5-5", "gpt-5.5"}, f.store.installation.PreferredModels)

	rec = f.do(t, http.MethodPost, "/admin/v1/preferred-models", `{"model":"`+f.localModel+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, http.MethodPost, "/admin/v1/preferred-models/remove", `{"model":"claude-opus-5-5"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(t, http.MethodGet, "/admin/v1/preferred-models", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"preferred":["gpt-5.5","`+f.localModel+`"]}`, rec.Body.String())

	rec = f.do(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":["claude-opus-4-0"]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a catalog model outside the routable universe is not selectable")
	assert.Equal(t, []string{"gpt-5.5", f.localModel}, f.store.installation.PreferredModels)

	rec = f.do(t, http.MethodPut, "/admin/v1/preferred-models", `{"preferred":[]}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"preferred":[]}`, rec.Body.String())
}

func TestProviders_IncludeLocalProvidersAndToggle(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})

	rec := f.do(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"`+f.localProv+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{f.localProv}, f.store.installation.ExcludedProviders)

	rec = f.do(t, http.MethodGet, "/admin/v1/providers", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[
		{"provider":"anthropic","enabled":true},
		{"provider":"`+f.localProv+`","enabled":false},
		{"provider":"openai","enabled":true}
	]`, rec.Body.String())

	rec = f.do(t, http.MethodGet, "/admin/v1/excluded-providers", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"available":["anthropic","`+f.localProv+`","openai"],"excluded":["`+f.localProv+`"],"env_override_active":false}`, rec.Body.String())

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-providers/remove", `{"provider":"`+f.localProv+`"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, f.store.installation.ExcludedProviders)

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"nope"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
