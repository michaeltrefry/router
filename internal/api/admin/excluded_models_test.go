package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weave-os/router/internal/api/admin"
	"weave-os/router/internal/auth"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/catalog"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type excludedModelsInstallationRepo struct {
	fastModeInstallationRepo
	excluded []string
}

func (r *excludedModelsInstallationRepo) UpdateExcludedModels(_ context.Context, _, _ string, models []string, _ []auth.RoutableModel) error {
	r.excluded = append([]string{}, models...)
	return nil
}

type excludedModelsBody struct {
	Available []struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Local    bool   `json:"local"`
	} `json:"available"`
	Excluded []string `json:"excluded"`
}

func registerTestLocalModel(t *testing.T, id string) string {
	t.Helper()
	provider := providers.LocalProviderName(id)
	require.NoError(t, catalog.RegisterLocalModels(catalog.Model{
		ID:        id,
		Tier:      catalog.TierMid,
		Providers: []catalog.ProviderBinding{{Provider: provider, UpstreamID: "upstream-" + id}},
	}))
	t.Cleanup(func() { catalog.UnregisterLocalModels(id) })
	return provider
}

func excludedModelsEngine(repo *excludedModelsInstallationRepo, installation *auth.Installation) *gin.Engine {
	gin.SetMode(gin.TestMode)
	authSvc := auth.NewService(repo, nil, nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Unix(0, 0) })
	inject := func(c *gin.Context) { c.Set("router_installation", installation) }
	engine := gin.New()
	engine.GET("/admin/v1/excluded-models", inject, admin.GetExcludedModelsHandler(authSvc, nil, nil))
	engine.PUT("/admin/v1/excluded-models", inject, admin.UpdateExcludedModelsHandler(authSvc, nil, nil, nil))
	return engine
}

func TestExcludedModelsHandlers_ListAndExcludeLocalModel(t *testing.T) {
	const id = "test-local-dashboard"
	provider := registerTestLocalModel(t, id)
	repo := &excludedModelsInstallationRepo{}
	engine := excludedModelsEngine(repo, &auth.Installation{ID: "inst-1"})

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/excluded-models", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var listed excludedModelsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	var found, cloudMarkedLocal bool
	for _, m := range listed.Available {
		if m.Model == id {
			found = true
			assert.Equal(t, provider, m.Provider)
			assert.True(t, m.Local, "a configured local model is flagged so the UI can group it")
		} else if m.Local {
			cloudMarkedLocal = true
		}
	}
	require.True(t, found, "the models page lists each configured local model")
	assert.False(t, cloudMarkedLocal, "only configured local models carry the local flag")

	body, err := json.Marshal(map[string][]string{"excluded": {id}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/excluded-models", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated excludedModelsBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	assert.Equal(t, []string{id}, updated.Excluded)
	assert.Equal(t, []string{id}, repo.excluded, "toggling a local model off persists it to the exclusion list")
}
