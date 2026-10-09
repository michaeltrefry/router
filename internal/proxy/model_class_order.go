package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/policy"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/router/turntype"
)

// reasonModelClassOrder is the decision reason of a turn served on an entry
// of its class's order list.
const reasonModelClassOrder = "model_class_order"

// ModelClassOrder lists, per class, the models a request naming that class
// falls back through, in order. Low is strict: a low-class turn is served by
// the first entry that can take it and by no other low model. High and mid
// keep the router's in-class pick and fall back through the list.
type ModelClassOrder map[catalog.Tier][]string

// ModelClassMembersContextKey carries the set of models a strict class order
// confines a request to; absent when the class has no strict order.
type ModelClassMembersContextKey struct{}

// WithModelClassOrder installs the per-class order lists.
func (s *Service) WithModelClassOrder(order ModelClassOrder) *Service {
	s.modelClassOrder = order
	return s
}

// WithClassRotation names the classes whose order list is rotated per session
// and serves every turn, instead of backing up the scorer's pick.
func (s *Service) WithClassRotation(classes ...catalog.Tier) *Service {
	s.classRotation = map[catalog.Tier]bool{}
	for _, c := range classes {
		s.classRotation[c] = true
	}
	return s
}

// ModelClassMembers returns the only models a request of class may be served
// on, or nil when every model of the class may serve it. Only the low order is
// strict.
func (s *Service) ModelClassMembers(class catalog.Tier) []string {
	if class != catalog.TierLow {
		return nil
	}
	return s.modelClassOrder[catalog.TierLow]
}

// classMembersAllow reports whether a strict class order, when the request
// carries one, lists model.
func classMembersAllow(ctx context.Context, model string) bool {
	members, ok := ctx.Value(ModelClassMembersContextKey{}).(map[string]struct{})
	if !ok {
		return true
	}
	_, listed := members[model]
	return listed
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
	if _, unsafe := req.SafetyExcludedModels[model]; unsafe {
		return router.Decision{}, false
	}
	if _, unsigned := req.UnsignedHistoryExcludedModels[model]; unsigned {
		return router.Decision{}, false
	}
	d := base
	d.Provider, d.Model = provider, model
	d.Effort = ""
	d.Metadata = nil
	d.Reason = reasonModelClassOrder
	return d, true
}

// classOrderCandidates resolves the request class's order entries that can
// take the turn, in order, skipping the models in skip. ok is false when the
// request names no class or the class has no order.
func (s *Service) classOrderCandidates(ctx context.Context, base router.Decision, tt turntype.TurnType, req router.Request, skip ...string) (candidates []router.Decision, ok bool) {
	class, classed := requestModelClass(ctx)
	order := s.modelClassOrder[class]
	if !classed || len(order) == 0 {
		return nil, false
	}
	return s.servableOrderEntries(ctx, base, tt, req, order, skip...), true
}

// servableOrderEntries resolves the entries of order that can take the turn,
// in order, skipping the models in skip.
func (s *Service) servableOrderEntries(ctx context.Context, base router.Decision, tt turntype.TurnType, req router.Request, order []string, skip ...string) (candidates []router.Decision) {
	for _, model := range order {
		if slices.Contains(skip, model) || catalog.IsLocal(model) && localRoutingDisabled(ctx) {
			continue
		}
		if d, servable := classOrderTarget(base, model, tt, req); servable {
			candidates = append(candidates, d)
		}
	}
	return candidates
}

// serveClassOrder puts res on the first candidate, keeping the rest as the
// turn's in-turn fallbacks.
func serveClassOrder(res *turnLoopResult, candidates []router.Decision) {
	res.Decision = candidates[0]
	res.LocalAlternates = candidates[1:]
	res.ClassOrdered = true
	res.Origin = policy.OverrideSourceDeployment
}

// orderedClassTurn reports whether the request's class picks its model from
// its order list rather than the scorer: the strict low order, or a class
// that rotates its list per session.
func (s *Service) orderedClassTurn(ctx context.Context) bool {
	class, classed := requestModelClass(ctx)
	if !classed || len(s.modelClassOrder[class]) == 0 {
		return false
	}
	return class == catalog.TierLow || s.classRotation[class]
}

// serveOrderedClass serves an ordered-class turn on the first servable entry
// of its list, rotated for a rotating class so the list starts at the
// session's slot; the entries after it are its fallbacks.
func (s *Service) serveOrderedClass(ctx context.Context, res *turnLoopResult, req router.Request, sessionKey [sessionpin.SessionKeyLen]byte) error {
	class, _ := requestModelClass(ctx)
	order := s.modelClassOrder[class]
	if s.classRotation[class] {
		order = rotateForSession(order, sessionKey)
	}
	candidates := s.servableOrderEntries(ctx, res.Decision, res.TurnType, req, order)
	if len(candidates) == 0 {
		return &ModelClassUnavailableError{Class: class, Err: cluster.ErrNoEligibleProvider}
	}
	serveClassOrder(res, candidates)
	return nil
}

// rotateForSession starts order at the session's slot, a stable hash of its
// key, so one session keeps one model every turn and sessions spread evenly
// across the list.
func rotateForSession(order []string, sessionKey [sessionpin.SessionKeyLen]byte) []string {
	start := int(binary.BigEndian.Uint64(sessionKey[:8]) % uint64(len(order)))
	return append(append([]string(nil), order[start:]...), order[:start]...)
}

// withClassBackup gives a high- or mid-class turn's automatic decision the
// rest of its class order as in-turn fallbacks: a pick that fails before
// output passes the turn to the next listed model, which reaches models the
// scorer cannot pick.
func (s *Service) withClassBackup(ctx context.Context, res *turnLoopResult, req router.Request) {
	if class, _ := requestModelClass(ctx); class == catalog.TierLow || res.ClassOrdered ||
		res.UsageBypass || res.CallerModelPassthrough || isUserForcedReason(res.Decision.Reason) {
		return
	}
	candidates, ordered := s.classOrderCandidates(ctx, res.Decision, res.TurnType, req,
		res.Decision.Model, res.SubstitutedFrom.Model, res.MappedDecision.Model)
	if !ordered || len(candidates) == 0 {
		return
	}
	res.LocalAlternates = candidates
	res.ClassOrdered = true
}

// rescueEmptyClass serves a high- or mid-class request whose routing found no
// candidate on the first order entry that can take it, since the order may
// list models the scorer cannot pick. It reports whether it served the turn.
func (s *Service) rescueEmptyClass(ctx context.Context, res *turnLoopResult, req router.Request, routeErr error) bool {
	if !errors.Is(routeErr, cluster.ErrNoEligibleProvider) && !errors.Is(routeErr, policy.ErrNoRoutableModels) {
		return false
	}
	// An org allowlist names its own fix, and a request the class already
	// refused (a caller-model passthrough outside it) is not router-chosen.
	var refused *ModelClassUnavailableError
	if errors.Is(routeErr, cluster.ErrAllowlistEmptiesPool) || errors.As(routeErr, &refused) {
		return false
	}
	candidates, ordered := s.classOrderCandidates(ctx, res.Decision, res.TurnType, req)
	if !ordered || len(candidates) == 0 {
		return false
	}
	serveClassOrder(res, candidates)
	return true
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

// classOrderedRoute applies the class order to a decision-only route (the
// /v1/route dry run): a low-class request reports its first servable entry,
// and a high- or mid-class request whose routing found nothing reports the
// first servable entry of its list.
func (s *Service) classOrderedRoute(ctx context.Context, req router.Request, decision router.Decision, err error) (router.Decision, error) {
	res := turnLoopResult{TurnType: turntype.MainLoop}
	if s.orderedClassTurn(ctx) {
		if orderErr := s.serveOrderedClass(ctx, &res, req, [sessionpin.SessionKeyLen]byte{}); orderErr != nil {
			return router.Decision{}, orderErr
		}
		return res.Decision, nil
	}
	if err != nil && s.rescueEmptyClass(ctx, &res, req, err) {
		return res.Decision, nil
	}
	return decision, err
}
