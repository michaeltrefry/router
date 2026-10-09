package proxy

// reasonMidTierSubstitute is the decision reason of a turn served by the
// mid-tier substitute in place of the router's own mid-tier pick.
const reasonMidTierSubstitute = "mid_tier_substitute"

// MidTierSubstitute serves automatically routed turns whose selected model is
// mid tier on one deployment-configured self-hosted model. It is the last
// substitution rule, matched on the tier of the router's own pick; see
// substituteLocal.
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

// midTierSubstitutable reports whether the turn's decision is the router's own
// automatic selection on a turn a local model may serve.
func midTierSubstitutable(res turnLoopResult) bool {
	if res.HardPinned || res.UsageBypass || res.CallerModelPassthrough || len(res.Purpose) > 0 ||
		isUserForcedReason(res.Decision.Reason) || res.Decision.Reason == reasonModelClassOrder {
		return false
	}
	return localServableTurn(res.TurnType)
}
