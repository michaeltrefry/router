package proxy

import (
	"context"
	"net/http"

	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/turntype"
)

// LocalTurnRoute serves the listed turn types on one deployment-configured
// self-hosted model.
type LocalTurnRoute struct {
	Provider  string
	Model     string
	TurnTypes []turntype.TurnType
}

// DefaultLocalTurnTypes are served locally when a route names no turn types.
var DefaultLocalTurnTypes = []turntype.TurnType{
	turntype.SubAgentDispatch,
	turntype.TitleGen,
	turntype.Probe,
	turntype.Recap,
}

// LocalTurnRoutable reports whether a local turn route may serve tt. Main-loop
// and tool-result turns keep normal routing.
func LocalTurnRoutable(tt turntype.TurnType) bool {
	if !localServableTurn(tt) {
		return false
	}
	switch tt {
	case turntype.SubAgentDispatch, turntype.TitleGen, turntype.Probe, turntype.Recap:
		return true
	default:
		return false
	}
}

// localServableTurn reports whether any local rule (turn route, mid-tier
// substitute, subscription fallback) may serve tt. Classifier and compaction
// turns never are: their verdict or summary governs the session that follows.
func localServableTurn(tt turntype.TurnType) bool {
	return tt != turntype.Classifier && tt != turntype.Compaction
}

// WithLocalTurnRoute installs the local turn route. Turn types that are not
// LocalTurnRoutable are dropped; an empty provider or model disables it.
func (s *Service) WithLocalTurnRoute(route LocalTurnRoute) *Service {
	s.localTurnProvider, s.localTurnModel, s.localTurnTypes = "", "", nil
	if route.Provider == "" || route.Model == "" {
		return s
	}
	types := make(map[turntype.TurnType]struct{}, len(route.TurnTypes))
	for _, tt := range route.TurnTypes {
		if LocalTurnRoutable(tt) {
			types[tt] = struct{}{}
		}
	}
	if len(types) == 0 {
		return s
	}
	s.localTurnProvider, s.localTurnModel, s.localTurnTypes = route.Provider, route.Model, types
	return s
}

// localTurnTarget returns the local model's binding when the route serves tt
// and this request may reach the model. Otherwise the turn routes exactly as
// it would with no route configured, so an installation that excluded the
// model, a request it cannot carry, or a deployment-wide disable all fall
// back to the existing path.
func (s *Service) localTurnTarget(tt turntype.TurnType, req router.Request) (provider, model string, ok bool) {
	if _, routed := s.localTurnTypes[tt]; !routed {
		return "", "", false
	}
	if !localModelServes(s.localTurnProvider, s.localTurnModel, req) {
		return "", "", false
	}
	return s.localTurnProvider, s.localTurnModel, true
}

// localModelServes reports whether a deployment-configured local model may take
// req on the router's behalf: the installation has not excluded it, its
// provider is enabled, it is not disabled for automatic routing, and the
// request fits its context window, carries no images it cannot read, and
// carries no tools when it is rated low for tool or agentic use.
func localModelServes(provider, model string, req router.Request) bool {
	return automaticServingIneligibility(provider, model, req) == ""
}

// automaticServingIneligibility names why a model the router serves on its
// own behalf may not take req, or returns "" when it may. The context check
// uses the full-body estimate and output reserve the ingress pre-filter
// applied to the scored roster, since text alone omits tool definitions.
func automaticServingIneligibility(provider, model string, req router.Request) string {
	if provider == "" || model == "" {
		return "unconfigured"
	}
	if _, excluded := req.ExcludedModels[model]; excluded {
		return "request_excluded"
	}
	if req.EnabledProviders != nil {
		if _, enabled := req.EnabledProviders[provider]; !enabled {
			return "provider_not_enabled"
		}
	}
	if !pinServesImages(sessionpin.Pin{Provider: provider, Model: model}, req) {
		return "not_image_capable"
	}
	if automaticallyDisabled(req, model) {
		return "automatic_routing_disabled"
	}
	fit := req.ContextFit
	if req.EstimatedInputTokens > catalog.ContextWindowForBinding(model, provider) ||
		!siblingFitsContext(model, provider, fit.OverflowTokens, fit.SignatureSavings, fit.OutputReserve) {
		return "context_window_exceeded"
	}
	if entry, known := catalog.ByID(model); known && req.HasTools &&
		(entry.ToolUseQuality == catalog.ToolUseLow || entry.AgenticUse == catalog.AgenticLow) {
		return "low_tool_rating"
	}
	return ""
}

// codexSubAgentHeader carries the kind of Codex thread a request comes from;
// codexSpawnedSubAgent marks a sub-agent the model spawned. codexThreadHeader
// names the thread; a main thread's equals its Session-Id.
const (
	codexSubAgentHeader  = "x-openai-subagent"
	codexSpawnedSubAgent = "collab_spawn"
	codexThreadHeader    = "Thread-Id"
)

// codexLocalSubAgentTurn reports whether a Codex spawned sub-agent turn on
// Responses ingress that Detect classified as normal work is served by the
// local turn route as sub-agent dispatch. Only a turn the local model accepts
// is reclassified; any other turn keeps Detect's result, so an ineligible
// request never gains the sub-agent hard pin. Codex's review, compaction and
// approval threads carry other header values and stay on normal routing.
func (s *Service) codexLocalSubAgentTurn(ctx context.Context, h http.Header, detected turntype.TurnType, req router.Request) bool {
	if (detected != turntype.MainLoop && detected != turntype.ToolResult) || localRoutingDisabled(ctx) {
		return false
	}
	if h.Get(codexSubAgentHeader) != codexSpawnedSubAgent {
		return false
	}
	if responses, _ := ctx.Value(responsesSurfaceContextKey{}).(bool); !responses || ClientIdentityFrom(ctx).ClientApp != ClientAppCodex {
		return false
	}
	_, _, ok := s.localTurnTarget(turntype.SubAgentDispatch, req)
	return ok
}
