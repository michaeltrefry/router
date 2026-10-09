package proxy

import "weave-os/router/internal/router"

// reasonLowTierSubstitute is the decision reason of a turn served by a local
// model in place of the router's own low-tier pick.
const reasonLowTierSubstitute = "low_tier_substitute"

// LowTierSubstitute serves automatically routed turns whose selected model is
// low tier on deployment-configured self-hosted models, tried in order: the
// first that can carry the request serves it, and one that fails before
// output passes the turn to the next, then to the router's pick.
type LowTierSubstitute struct {
	Targets []LocalTarget
}

// LocalTarget is one self-hosted model and its local provider.
type LocalTarget struct {
	Provider string
	Model    string
}

// WithLowTierSubstitute installs the low-tier substitute; no targets disables
// it, and incomplete targets are dropped.
func (s *Service) WithLowTierSubstitute(sub LowTierSubstitute) *Service {
	s.lowTierTargets = nil
	for _, t := range sub.Targets {
		if t.Provider != "" && t.Model != "" {
			s.lowTierTargets = append(s.lowTierTargets, t)
		}
	}
	return s
}

// lowTierCandidates returns the low-tier targets that may serve req, in
// configured order, as decisions retargeted from pick.
func (s *Service) lowTierCandidates(pick router.Decision, req router.Request) []router.Decision {
	var out []router.Decision
	for _, t := range s.lowTierTargets {
		if !localModelServes(t.Provider, t.Model, req) {
			continue
		}
		d := pick
		d.Provider, d.Model = t.Provider, t.Model
		d.Effort = ""
		d.Reason = reasonLowTierSubstitute
		out = append(out, d)
	}
	return out
}
