package proxy

import (
	"context"
	"slices"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/turntype"
)

// reasonModelClassOrder is the decision reason of a turn a low-class request
// served on the first entry of the class order that could take it.
const reasonModelClassOrder = "model_class_order"

// ModelClassOrder lists, per class, the models a request naming that class
// falls back through, in order. Low is strict: a low-class turn is served by
// the first entry that can take it and by no other low model. High and mid
// keep the router's in-class pick and use the list as its backup order.
type ModelClassOrder map[catalog.Tier][]string

// WithModelClassOrder installs the per-class order lists.
func (s *Service) WithModelClassOrder(order ModelClassOrder) *Service {
	s.modelClassOrder = order
	return s
}

// classOrderTarget retargets base onto an order entry for req, or returns
// ok=false when the entry cannot take the turn. The plan resolver authorizes
// it under the deployment override source, as for the local substitutes.
func classOrderTarget(base router.Decision, model string, tt turntype.TurnType, req router.Request) (router.Decision, bool) {
	provider := providers.LocalProviderName(model)
	if !catalog.IsLocal(model) {
		binding, found := catalog.ResolveBindingWithCustom(model, req.EnabledProviders, req.CustomBindings)
		if !found {
			return router.Decision{}, false
		}
		provider = binding.Provider
	} else if !localServableTurn(tt) {
		return router.Decision{}, false
	}
	if automaticServingIneligibility(provider, model, req) != "" {
		return router.Decision{}, false
	}
	d := base
	d.Provider, d.Model = provider, model
	d.Reason = reasonModelClassOrder
	return d, true
}

// lowClassOrderDecision serves a low-class request on the first entry of the
// low order that can take the turn; the entries after it are its fallbacks in
// order. ok is false when the request is not low class or no order is set; a
// configured order with no servable entry still answers, with no candidate,
// so the request fails in its class rather than reaching another low model.
func (s *Service) lowClassOrderDecision(ctx context.Context, base router.Decision, tt turntype.TurnType, req router.Request) (first router.Decision, rest []router.Decision, ok bool) {
	class, classed := requestModelClass(ctx)
	order := s.modelClassOrder[catalog.TierLow]
	if !classed || class != catalog.TierLow || len(order) == 0 {
		return router.Decision{}, nil, false
	}
	var candidates []router.Decision
	for _, model := range order {
		if catalog.IsLocal(model) && localRoutingDisabled(ctx) {
			continue
		}
		if d, servable := classOrderTarget(base, model, tt, req); servable {
			candidates = append(candidates, d)
		}
	}
	if len(candidates) == 0 {
		return router.Decision{}, nil, true
	}
	// Each entry's in-turn rescue walks the entries after it, in order.
	for i := range candidates {
		rescue := make([]string, 0, len(candidates)-i-1)
		for _, d := range candidates[i+1:] {
			rescue = append(rescue, d.Model)
		}
		candidates[i].Metadata = &router.RoutingMetadata{RescueModels: rescue, RosterFailover: true}
	}
	return candidates[0], candidates[1:], true
}

// withClassBackupOrder makes the class order the in-turn rescue order of a
// high- or mid-class decision: sibling failover walks the listed models after
// the one serving, which reaches models the scorer cannot pick.
func (s *Service) withClassBackupOrder(ctx context.Context, res *turnLoopResult) {
	class, classed := requestModelClass(ctx)
	order := s.modelClassOrder[class]
	if !classed || class == catalog.TierLow || len(order) == 0 || res.Decision.Model == "" {
		return
	}
	serving := []string{res.Decision.Model, res.SubstitutedFrom.Model, res.MappedDecision.Model}
	rescue := make([]string, 0, len(order))
	for _, model := range order {
		if !slices.Contains(serving, model) {
			rescue = append(rescue, model)
		}
	}
	md := router.RoutingMetadata{}
	if res.Decision.Metadata != nil {
		md = *res.Decision.Metadata
	}
	md.RescueModels = rescue
	md.RosterFailover = true
	res.Decision.Metadata = &md
}

// legacyForcePinned reports a thread-scoped /force-model pin on the turn's pin
// role, which outranks the class order like a session force does.
func (s *Service) legacyForcePinned(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) bool {
	if s.pinStore == nil {
		return false
	}
	pin, found := s.loadPin(ctx, sessionKey, role)
	return found && isUserForcedReason(pin.Reason)
}
