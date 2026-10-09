package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/api/admin"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/catalog"
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

func (r modelSelectionInstallationRepo) Get(context.Context, string, string) (*auth.Installation, error) {
	inst := r.store.installation
	return &inst, nil
}

// The writes below mirror the SQL contract (see
// db/queries/model_router_installations.sql), which smoke runs against Postgres.

func (r modelSelectionInstallationRepo) UpdateExcludedModels(_ context.Context, _, _ string, models []string, universe []auth.RoutableModel) error {
	if universe != nil && !routableLeft(universe, models, r.store.installation.ExcludedProviders) {
		return auth.ErrNoRoutableModels
	}
	r.store.writes++
	r.store.installation.ExcludedModels = append([]string{}, models...)
	return nil
}

func (r modelSelectionInstallationRepo) UpdateExcludedProviders(_ context.Context, _, _ string, names []string, universe []auth.RoutableModel) error {
	if universe != nil && !routableLeft(universe, r.store.installation.ExcludedModels, names) {
		return auth.ErrNoRoutableModels
	}
	r.store.writes++
	r.store.installation.ExcludedProviders = append([]string{}, names...)
	return nil
}

func (r modelSelectionInstallationRepo) UpdatePreferredModels(_ context.Context, _, _ string, models []string) error {
	r.store.writes++
	r.store.installation.PreferredModels = append([]string{}, models...)
	return nil
}

func (r modelSelectionInstallationRepo) EditSelectionItem(_ context.Context, _, _ string, edit auth.SelectionItemEdit) error {
	inst := &r.store.installation
	list := map[auth.SelectionList]*[]string{
		auth.SelectionExcludedModels:    &inst.ExcludedModels,
		auth.SelectionExcludedProviders: &inst.ExcludedProviders,
		auth.SelectionPreferredModels:   &inst.PreferredModels,
	}[edit.List]
	present := slices.Contains(*list, edit.Item)
	if present == edit.Add {
		return nil
	}
	if edit.Add && edit.Universe != nil {
		models, provs := inst.ExcludedModels, inst.ExcludedProviders
		if edit.List == auth.SelectionExcludedModels {
			models = append(slices.Clone(models), edit.Item)
		} else {
			provs = append(slices.Clone(provs), edit.Item)
		}
		if !routableLeft(edit.Universe, models, provs) {
			return auth.ErrNoRoutableModels
		}
	}
	out := []string{}
	for _, v := range *list {
		if slices.Contains(edit.Keep, v) && v != edit.Item {
			out = append(out, v)
		}
	}
	if edit.Add {
		out = append(out, edit.Item)
	}
	r.store.writes++
	*list = out
	return nil
}

func routableLeft(universe []auth.RoutableModel, excludedModels, excludedProviders []string) bool {
	for _, m := range universe {
		if !slices.Contains(excludedModels, m.Model) && !slices.Contains(excludedProviders, m.Provider) {
			return true
		}
	}
	return false
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
	g.PUT("/excluded-models", admin.UpdateExcludedModelsHandler(authSvc, deployed, routable, override))
	g.POST("/excluded-models", admin.AddExcludedModelHandler(authSvc, deployed, routable, override))
	g.POST("/excluded-models/remove", admin.RemoveExcludedModelHandler(authSvc, deployed, routable, override))
	g.GET("/preferred-models", admin.GetPreferredModelsHandler(authSvc))
	g.PUT("/preferred-models", admin.UpdatePreferredModelsHandler(authSvc, routable))
	g.POST("/preferred-models", admin.AddPreferredModelHandler(authSvc, routable))
	g.POST("/preferred-models/remove", admin.RemovePreferredModelHandler(authSvc, routable))
	g.GET("/providers", admin.GetProvidersHandler(authSvc, deployed, routable, override))
	g.GET("/excluded-providers", admin.GetExcludedProvidersHandler(authSvc, deployed, routable, override))
	g.PUT("/excluded-providers", admin.UpdateExcludedProvidersHandler(authSvc, deployed, routable, override))
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
	Class    string `json:"class"`
}

func TestGetModels_ListsRoutableUniverseWithEnabledState(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	f.store.installation.ExcludedModels = []string{"claude-opus-5"}

	rec := f.do(t, http.MethodGet, "/admin/v1/models", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []modelRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Equal(t, []modelRow{
		{Model: "claude-opus-5", Provider: providers.ProviderAnthropic, Enabled: false, Class: "high"},
		{Model: "claude-opus-5-5", Provider: providers.ProviderAnthropic, Enabled: true, Class: "high"},
		{Model: f.localModel, Provider: f.localProv, Enabled: true, Local: true, Class: "mid"},
		{Model: "gpt-5.5", Provider: providers.ProviderOpenAI, Enabled: true},
	}, rows, "the routable universe, mapping target and local model included, sorted by provider then model; class is the tier")
}

// The class follows the deployment's model_tiers, not the static catalog.
func TestGetModels_ClassFollowsDeploymentTiers(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	t.Cleanup(catalog.RestoreTiers)
	require.NoError(t, catalog.RetierModels(map[string]catalog.Tier{"claude-opus-5-5": catalog.TierMid}))

	rec := f.do(t, http.MethodGet, "/admin/v1/models", "")

	var rows []modelRow
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	classes := map[string]string{}
	for _, r := range rows {
		classes[r.Model] = r.Class
	}
	assert.Equal(t, "mid", classes["claude-opus-5-5"])
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

func TestExclusions_RejectLeavingNothingRoutable(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	everyModel := `["claude-opus-5","claude-opus-5-5","gpt-5.5","` + f.localModel + `"]`

	rec := f.do(t, http.MethodPut, "/admin/v1/excluded-models", `{"excluded":`+everyModel+`}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "would leave no model the router can route to")
	assert.Empty(t, f.store.installation.ExcludedModels, "the dashboard PUT is refused whole")

	for _, m := range []string{"claude-opus-5", "claude-opus-5-5", f.localModel} {
		rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"`+m+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"gpt-5.5"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the last enabled model cannot be disabled")
	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"`+providers.ProviderOpenAI+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "nor can the provider of the last enabled model")
	rec = f.do(t, http.MethodPut, "/admin/v1/excluded-providers", `{"excluded":["`+providers.ProviderOpenAI+`"]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "would leave no model the router can route to")
	assert.Empty(t, f.store.installation.ExcludedProviders)

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"claude-opus-5"}`)
	assert.Equal(t, http.StatusOK, rec.Code, "re-adding an excluded model stays idempotent")
}

func TestSelectionItems_DropStaleEntriesInsteadOfRejecting(t *testing.T) {
	f := newModelSelectionFixture(t, fakeExclusionOverride{})
	f.store.installation.PreferredModels = []string{"retired-model", "gpt-5.5"}
	f.store.installation.ExcludedModels = []string{"retired-model"}

	rec := f.do(t, http.MethodPost, "/admin/v1/preferred-models", `{"model":"`+f.localModel+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"preferred":["gpt-5.5","`+f.localModel+`"]}`, rec.Body.String())

	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-models", `{"model":"gpt-5.5"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"gpt-5.5"}, f.store.installation.ExcludedModels)
}

// A model stays routable while any selectable provider serving it is enabled:
// z-ai/glm-5.3-flash lists deepinfra first, which this deployment cannot
// select, and is served here by fireworks or together.
func TestProviderExclusions_CountEveryServingProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &modelSelectionStore{installation: auth.Installation{ID: "inst-1", ExternalID: "ext-1"}}
	authSvc := auth.NewService(modelSelectionInstallationRepo{store: store}, modelSelectionKeyRepo{store: store},
		nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Unix(0, 0) })
	const model = "z-ai/glm-5.3-flash"
	routable := fakeRoutable{model: {}}
	deployed := fakeDeployed{
		{Model: model, Provider: providers.ProviderFireworks},
		{Model: model, Provider: providers.ProviderTogether},
	}
	engine := gin.New()
	g := engine.Group("/admin/v1", middleware.WithAdminOrAuth(authSvc, false))
	g.POST("/excluded-providers", admin.AddExcludedProviderHandler(authSvc, deployed, routable, fakeExclusionOverride{}))
	f := modelSelectionFixture{engine: engine, store: store}

	rec := f.do(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"`+providers.ProviderFireworks+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, "together still serves the model: %s", rec.Body.String())
	rec = f.do(t, http.MethodPost, "/admin/v1/excluded-providers", `{"provider":"`+providers.ProviderTogether+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "nothing selectable serves the model once both are excluded")
	assert.Equal(t, []string{providers.ProviderFireworks}, store.installation.ExcludedProviders)
}
