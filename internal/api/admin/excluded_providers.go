package admin

import (
	"net/http"
	"sort"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// ProviderExclusionOverrideSource reports the deployment-wide ROUTER_EXCLUDED_PROVIDERS
// override, if active. Implemented by *proxy.Service.
type ProviderExclusionOverrideSource interface {
	HasExcludedProvidersOverride() bool
	ExcludedProvidersOverride() []string
}

type excludedProvidersResponse struct {
	Available         []string `json:"available"`
	Excluded          []string `json:"excluded"`
	EnvOverrideActive bool     `json:"env_override_active"`
}

type updateExcludedProvidersRequest struct {
	Excluded []string `json:"excluded"`
}

// deployedProvidersDTO returns distinct provider names from the deployed-models
// registry, sorted, so GET and PUT responses can't drift apart.
func deployedProvidersDTO(models DeployedModelsSource) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, e := range models.DefaultDeployedModels() {
		if _, dup := seen[e.Provider]; dup {
			continue
		}
		seen[e.Provider] = struct{}{}
		out = append(out, e.Provider)
	}
	sort.Strings(out)
	return out
}

// GetExcludedProvidersHandler returns selectable providers and the installation's
// exclusion list. `env_override_active` tells the UI to render read-only.
func GetExcludedProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		envActive := override != nil && override.HasExcludedProvidersOverride()
		var excluded []string
		if envActive {
			excluded = override.ExcludedProvidersOverride()
		} else {
			excluded = append([]string{}, installation.ExcludedProviders...)
			sort.Strings(excluded)
		}
		if excluded == nil {
			excluded = []string{}
		}

		c.JSON(http.StatusOK, excludedProvidersResponse{
			Available:         selectableProviders(models, routable),
			Excluded:          excluded,
			EnvOverrideActive: envActive,
		})
	}
}

// UpdateExcludedProvidersHandler replaces the installation's exclusion list.
// 400 on unknown providers or a list that leaves nothing routable; 403 if the
// env override is active.
func UpdateExcludedProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if override != nil && override.HasExcludedProvidersOverride() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Exclusion list is pinned by ROUTER_EXCLUDED_PROVIDERS; clear the env var to edit.",
			})
			return
		}

		var req updateExcludedProvidersRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		available := selectableProviders(models, routable)
		allowed := make(map[string]struct{}, len(available))
		for _, p := range available {
			allowed[p] = struct{}{}
		}

		stored, err := authSvc.SetInstallationExcludedProviders(c.Request.Context(), installation.ExternalID, installation.ID, req.Excluded, allowed, routableUniverse(models, routable))
		if !respondProviderSelectionError(c, err) {
			return
		}

		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedProvidersResponse{
			Available: available,
			Excluded:  stored,
		})
	}
}

var _ ProviderExclusionOverrideSource = (*proxy.Service)(nil)
