package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubClassGate map[catalog.Tier][]string

func (g stubClassGate) ModelClassMembers(class catalog.Tier) []string { return g[class] }

func runModelClassMiddleware(t *testing.T, headers map[string]string) (int, catalog.Tier, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.WithModelClass(stubClassGate{}))
	var class catalog.Tier
	var ok bool
	engine.POST("/v1/messages", func(c *gin.Context) {
		class, ok = c.Request.Context().Value(proxy.ModelClassContextKey{}).(catalog.Tier)
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code, class, ok
}

func TestModelClass_StashesClassCaseInsensitively(t *testing.T) {
	status, class, ok := runModelClassMiddleware(t, map[string]string{proxy.ModelClassHeader: " High "})
	require.Equal(t, http.StatusOK, status)
	require.True(t, ok)
	assert.Equal(t, catalog.TierHigh, class)
}

func TestModelClass_AbsentHeaderIsNoOp(t *testing.T) {
	status, _, ok := runModelClassMiddleware(t, nil)
	require.Equal(t, http.StatusOK, status)
	assert.False(t, ok)
}

func TestModelClass_RejectsUnknownClass(t *testing.T) {
	status, _, ok := runModelClassMiddleware(t, map[string]string{proxy.ModelClassHeader: "bogus"})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.False(t, ok)
}

func TestModelClass_RejectsClassWithForcedModel(t *testing.T) {
	status, _, ok := runModelClassMiddleware(t, map[string]string{
		proxy.ModelClassHeader: "mid",
		proxy.ForceModelHeader: "claude-opus-5-5",
	})
	assert.Equal(t, http.StatusBadRequest, status)
	assert.False(t, ok)
}

func TestModelClass_StashesStrictOrderMembers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.WithModelClass(stubClassGate{catalog.TierLow: {"local-a", "gpt-5.6-terra"}}))
	var members map[string]struct{}
	engine.POST("/v1/messages", func(c *gin.Context) {
		members, _ = c.Request.Context().Value(proxy.ModelClassMembersContextKey{}).(map[string]struct{})
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set(proxy.ModelClassHeader, "low")
	engine.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, map[string]struct{}{"local-a": {}, "gpt-5.6-terra": {}}, members)
}
