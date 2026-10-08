package admin

import (
	"errors"
	"net/http"
	"sort"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/router/catalog"

	"github.com/gin-gonic/gin"
)

const noRoutableModelsMessage = "That change would leave no model the router can route to. Keep at least one model enabled on an enabled provider."

type modelStatusDTO struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Local    bool   `json:"local,omitempty"`
}

type providerStatusDTO struct {
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
}

type preferredModelsResponse struct {
	Preferred []string `json:"preferred"`
}

type modelSelectionItemRequest struct {
	Model string `json:"model"`
}

type providerSelectionItemRequest struct {
	Provider string `json:"provider"`
}

// GetModelsHandler returns every model this deployment can route — catalog
// routing targets, configured local models and model-mapping targets — with
// the installation's effective enabled state.
func GetModelsHandler(authSvc *auth.Service, routable RoutableModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		excluded := installation.ExcludedModels
		if override != nil && override.HasExcludedModelsOverride() {
			excluded = override.ExcludedModelsOverride()
		}
		excludedSet := stringsSet(excluded)
		available := selectableModelsDTO(routable)
		out := make([]modelStatusDTO, 0, len(available))
		for _, model := range available {
			_, isExcluded := excludedSet[model.Model]
			out = append(out, modelStatusDTO{
				Model:    model.Model,
				Provider: model.Provider,
				Enabled:  !isExcluded,
				Local:    model.Local,
			})
		}
		c.JSON(http.StatusOK, out)
	}
}

// GetProvidersHandler returns every selectable provider with its effective enabled state.
func GetProvidersHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		excluded := installation.ExcludedProviders
		if override != nil && override.HasExcludedProvidersOverride() {
			excluded = override.ExcludedProvidersOverride()
		}
		excludedSet := stringsSet(excluded)
		available := selectableProviders(models, routable)
		out := make([]providerStatusDTO, 0, len(available))
		for _, provider := range available {
			_, isExcluded := excludedSet[provider]
			out = append(out, providerStatusDTO{
				Provider: provider,
				Enabled:  !isExcluded,
			})
		}
		c.JSON(http.StatusOK, out)
	}
}

// GetPreferredModelsHandler returns the installation's ordered model priority ranking.
func GetPreferredModelsHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		preferred := append([]string{}, installation.PreferredModels...)
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: preferred})
	}
}

// UpdatePreferredModelsHandler replaces the installation's ordered model priority ranking.
func UpdatePreferredModelsHandler(authSvc *auth.Service, routable RoutableModelsSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req preferredModelsResponse
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		stored, err := authSvc.SetInstallationPreferredModels(
			c.Request.Context(),
			installation.ExternalID,
			installation.ID,
			req.Preferred,
			selectableModelSet(routable),
		)
		if !respondModelSelectionError(c, err, "Failed to update preferred models.") {
			return
		}
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: stored})
	}
}

// AddExcludedModelHandler adds one model to the installation exclusion list.
func AddExcludedModelHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedModelItemHandler(authSvc, models, routable, override, true)
}

// RemoveExcludedModelHandler removes one model from the installation exclusion list.
func RemoveExcludedModelHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedModelItemHandler(authSvc, models, routable, override, false)
}

// updateExcludedModelItemHandler edits the same list, validated against the
// same catalog, as the dashboard's PUT /excluded-models, so the two agree.
func updateExcludedModelItemHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ExclusionOverrideSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if modelExclusionOverrideActive(c, override) {
			return
		}

		var req modelSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		available := fullCatalogDTO()
		stored, err := authSvc.EditInstallationSelection(c.Request.Context(), installation.ExternalID, installation.ID, auth.SelectionItemEdit{
			List:     auth.SelectionExcludedModels,
			Item:     req.Model,
			Add:      add,
			Keep:     listingModels(available),
			Universe: routableUniverse(models, routable),
		})
		if !respondModelSelectionError(c, err, "Failed to update excluded models.") {
			return
		}
		sort.Strings(stored)
		c.JSON(http.StatusOK, excludedModelsResponse{
			Available: available,
			Excluded:  stored,
		})
	}
}

// AddPreferredModelHandler appends one model to the ordered priority ranking.
func AddPreferredModelHandler(authSvc *auth.Service, routable RoutableModelsSource) gin.HandlerFunc {
	return updatePreferredModelItemHandler(authSvc, routable, true)
}

// RemovePreferredModelHandler removes one model from the priority ranking.
func RemovePreferredModelHandler(authSvc *auth.Service, routable RoutableModelsSource) gin.HandlerFunc {
	return updatePreferredModelItemHandler(authSvc, routable, false)
}

func updatePreferredModelItemHandler(authSvc *auth.Service, routable RoutableModelsSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}

		var req modelSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		stored, err := authSvc.EditInstallationSelection(c.Request.Context(), installation.ExternalID, installation.ID, auth.SelectionItemEdit{
			List: auth.SelectionPreferredModels,
			Item: req.Model,
			Add:  add,
			Keep: listingModels(selectableModelsDTO(routable)),
		})
		if !respondModelSelectionError(c, err, "Failed to update preferred models.") {
			return
		}
		c.JSON(http.StatusOK, preferredModelsResponse{Preferred: stored})
	}
}

// AddExcludedProviderHandler adds one provider to the installation exclusion list.
func AddExcludedProviderHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedProviderItemHandler(authSvc, models, routable, override, true)
}

// RemoveExcludedProviderHandler removes one provider from the installation exclusion list.
func RemoveExcludedProviderHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource) gin.HandlerFunc {
	return updateExcludedProviderItemHandler(authSvc, models, routable, override, false)
}

func updateExcludedProviderItemHandler(authSvc *auth.Service, models DeployedModelsSource, routable RoutableModelsSource, override ProviderExclusionOverrideSource, add bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		installation, ok := resolveInstallation(c, authSvc)
		if !ok {
			return
		}
		if providerExclusionOverrideActive(c, override) {
			return
		}

		var req providerSelectionItemRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid request body."})
			return
		}

		available := selectableProviders(models, routable)
		stored, err := authSvc.EditInstallationSelection(c.Request.Context(), installation.ExternalID, installation.ID, auth.SelectionItemEdit{
			List:     auth.SelectionExcludedProviders,
			Item:     req.Provider,
			Add:      add,
			Keep:     available,
			Universe: routableUniverse(models, routable),
		})
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

func modelExclusionOverrideActive(c *gin.Context, override ExclusionOverrideSource) bool {
	if override == nil || !override.HasExcludedModelsOverride() {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "Exclusion list is pinned by ROUTER_EXCLUDED_MODELS; clear the env var to edit.",
	})
	return true
}

func providerExclusionOverrideActive(c *gin.Context, override ProviderExclusionOverrideSource) bool {
	if override == nil || !override.HasExcludedProvidersOverride() {
		return false
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": "Exclusion list is pinned by ROUTER_EXCLUDED_PROVIDERS; clear the env var to edit.",
	})
	return true
}

// selectableModelsDTO lists the deployment's routable universe, falling back
// to the whole catalog when no universe is configured (the same fallback the
// router applies when routing).
func selectableModelsDTO(routable RoutableModelsSource) []deployedModelDTO {
	var universe map[string]struct{}
	if routable != nil {
		universe = routable.RoutableModels()
	}
	if universe == nil {
		return fullCatalogDTO()
	}
	out := make([]deployedModelDTO, 0, len(universe))
	for _, row := range fullCatalogDTO() {
		if _, ok := universe[row.Model]; ok {
			out = append(out, row)
		}
	}
	return out
}

// routableUniverse is the set the zero-routable guard checks an exclusion
// change against: each selectable model paired with every selectable provider
// that serves it, so excluding one provider leaves a model its other providers
// serve. Selectable providers are the scorer's plus local ones; a model none of
// them serves is never picked automatically, so it keeps nothing routable.
// Without the scorer's deployment there is nothing to judge against and the
// guard is off (nil).
func routableUniverse(models DeployedModelsSource, routable RoutableModelsSource) []auth.RoutableModel {
	if models == nil {
		return nil
	}
	providerSet := stringsSet(selectableProviders(models, routable))
	out := []auth.RoutableModel{}
	for _, row := range selectableModelsDTO(routable) {
		for _, b := range catalog.EnumerateBindings(row.Model, providerSet) {
			out = append(out, auth.RoutableModel{Model: row.Model, Provider: b.Provider})
		}
	}
	return out
}

func listingModels(rows []deployedModelDTO) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Model)
	}
	return out
}

func selectableModelSet(routable RoutableModelsSource) map[string]struct{} {
	out := make(map[string]struct{})
	for _, row := range selectableModelsDTO(routable) {
		out[row.Model] = struct{}{}
	}
	return out
}

// selectableProviders is the scorer's deployed providers plus the providers of
// routable local models, which no scorer artifact lists.
func selectableProviders(models DeployedModelsSource, routable RoutableModelsSource) []string {
	out := []string{}
	if models != nil {
		out = deployedProvidersDTO(models)
	}
	seen := stringsSet(out)
	for _, row := range selectableModelsDTO(routable) {
		if !catalog.IsLocal(row.Model) {
			continue
		}
		if _, dup := seen[row.Provider]; dup {
			continue
		}
		seen[row.Provider] = struct{}{}
		out = append(out, row.Provider)
	}
	sort.Strings(out)
	return out
}

func stringsSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func respondModelSelectionError(c *gin.Context, err error, failureMessage string) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrUnknownModel) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if errors.Is(err, auth.ErrNoRoutableModels) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": noRoutableModelsMessage})
		return false
	}
	observability.FromGin(c).Error(failureMessage, "err", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": failureMessage})
	return false
}

func respondProviderSelectionError(c *gin.Context, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrUnknownProvider) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if errors.Is(err, auth.ErrNoRoutableModels) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": noRoutableModelsMessage})
		return false
	}
	observability.FromGin(c).Error("Failed to update excluded providers", "err", err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Failed to update excluded providers."})
	return false
}
