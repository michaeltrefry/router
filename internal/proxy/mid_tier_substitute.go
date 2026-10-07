package proxy

import (
	"context"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/policy"
)

// reasonMidTierSubstitute is the decision reason of a turn served by the
// mid-tier substitute in place of the router's own mid-tier pick.
const reasonMidTierSubstitute = "mid_tier_substitute"

// MidTierSubstitute serves automatically routed turns whose selected model is
// mid tier on one deployment-configured self-hosted model.
type MidTierSubstitute struct {
	Provider string
	Model    string
}

// WithMidTierSubstitute installs the mid-tier substitute; an empty provider or
// model disables substitution.
func (s *Service) WithMidTierSubstitute(sub MidTierSubstitute) *Service {
	s.midTierProvider, s.midTierModel = "", ""
	if sub.Provider == "" || sub.Model == "" {
		return s
	}
	s.midTierProvider, s.midTierModel = sub.Provider, sub.Model
	return s
}

// substituteMidTier replaces a router-chosen mid-tier decision with the local
// substitute after every routing branch has run, so session pins, planner
// state and HMM history keep the router's own pick: a pinned session lands on
// the substitute again each turn, and disabling substitution returns it to the
// pinned model. Forced, hard-pinned, utility, bypassed and policy-pinned turns
// are never substituted, and a request the local model cannot take keeps the
// original decision.
func (s *Service) substituteMidTier(ctx context.Context, res *turnLoopResult, req router.Request) {
	if s.midTierModel == "" || localRoutingDisabled(ctx) || !midTierSubstitutable(*res) {
		return
	}
	// A mapped selection is judged by the router's own pick, which session
	// state keeps as the turn's selection.
	original := res.Decision
	if res.SubstitutionReason == reasonModelMapping {
		original = res.SubstitutedFrom
	}
	if catalog.TierFor(original.Model) != catalog.TierMid || original.Model == s.midTierModel {
		return
	}
	if _, pinned := router.HonouredPolicyPin(ctx); pinned {
		return
	}
	if !localModelServes(s.midTierProvider, s.midTierModel, req) {
		return
	}
	// Retarget a copy of the routed decision, keeping its routing metadata;
	// the plan resolver authorizes the new target under the deployment
	// override source. The original's effort is the replaced model's knob.
	substitute := res.Decision
	substitute.Provider, substitute.Model = s.midTierProvider, s.midTierModel
	substitute.Effort = ""
	substitute.Reason = reasonMidTierSubstitute
	res.SubstitutedFrom = original
	res.SubstitutionReason = reasonMidTierSubstitute
	res.Decision = substitute
	res.Origin = policy.OverrideSourceDeployment
	observability.FromContext(ctx).Info("Mid-tier substitute served turn",
		"turn_type", string(res.TurnType),
		"original_model", original.Model,
		"original_provider", original.Provider,
		"original_reason", original.Reason,
		"mapped_model", res.MappedDecision.Model,
		"substitute_model", s.midTierModel,
		"substitute_provider", s.midTierProvider,
	)
}

// midTierSubstitutable reports whether the turn's decision is the router's own
// automatic selection on a turn a local model may serve.
func midTierSubstitutable(res turnLoopResult) bool {
	if res.HardPinned || res.UsageBypass || res.CallerModelPassthrough || len(res.Purpose) > 0 ||
		isUserForcedReason(res.Decision.Reason) {
		return false
	}
	return localServableTurn(res.TurnType)
}
