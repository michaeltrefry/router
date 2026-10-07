package proxy

import (
	"context"
	"path"
	"slices"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/policy"
)

// reasonSubstitutionRule is the decision reason of a turn a model-pattern
// substitution rule served on a local model.
const reasonSubstitutionRule = "substitution_rule"

// SubstitutionRule serves an automatic selection whose served model matches
// Match, a path.Match glob over the model ID such as "gpt-*-luna", on one
// deployment-configured local model.
type SubstitutionRule struct {
	Match    string
	Provider string
	Model    string
}

// WithSubstitutionRules installs the model-pattern substitution rules, in
// evaluation order; incomplete rules are dropped.
func (s *Service) WithSubstitutionRules(rules []SubstitutionRule) *Service {
	s.substitutionRules = slices.DeleteFunc(slices.Clone(rules), func(r SubstitutionRule) bool {
		return r.Match == "" || r.Provider == "" || r.Model == ""
	})
	return s
}

// localSubstitutionReason reports whether reason names a rule that serves the
// router's selection on a local model every turn it matches.
func localSubstitutionReason(reason string) bool {
	return reason == reasonSubstitutionRule || reason == reasonMidTierSubstitute
}

// substitutionFor returns the first rule matching the turn: the substitution
// rules in configured order against the served (post-mapping) model, then the
// mid-tier substitute against the tier of the router's own pick.
func (s *Service) substitutionFor(pick, served router.Decision) (provider, model, reason string, ok bool) {
	for _, rule := range s.substitutionRules {
		if matched, _ := path.Match(rule.Match, served.Model); matched {
			return rule.Provider, rule.Model, reasonSubstitutionRule, true
		}
	}
	if s.midTierModel != "" && catalog.TierFor(pick.Model) == catalog.TierMid {
		return s.midTierProvider, s.midTierModel, reasonMidTierSubstitute, true
	}
	return "", "", "", false
}

// substituteLocal replaces a router-chosen decision with the local model of
// the first matching substitution rule after every routing branch has run, so
// session pins, planner state and HMM history keep the router's own pick: a
// pinned session lands on the substitute again each turn, and disabling the
// rule returns it to the pinned model. Forced, hard-pinned, utility, bypassed
// and policy-pinned turns are never substituted, and a request the matched
// rule's local model cannot take keeps the (mapped) decision; later rules are
// not tried.
func (s *Service) substituteLocal(ctx context.Context, res *turnLoopResult, req router.Request) {
	if (s.midTierModel == "" && len(s.substitutionRules) == 0) || localRoutingDisabled(ctx) || !midTierSubstitutable(*res) {
		return
	}
	// A mapped selection is judged by the router's own pick, which session
	// state keeps as the turn's selection.
	original := res.Decision
	if res.SubstitutionReason == reasonModelMapping {
		original = res.SubstitutedFrom
	}
	provider, model, reason, ok := s.substitutionFor(original, res.Decision)
	if !ok || res.Decision.Model == model || providers.IsLocalProvider(res.Decision.Provider) {
		return
	}
	if _, pinned := router.HonouredPolicyPin(ctx); pinned {
		return
	}
	if why := automaticServingIneligibility(provider, model, req); why != "" {
		observability.FromContext(ctx).Info("Local substitute skipped; serving the matched model",
			"reason", why,
			"substitution_reason", reason,
			"turn_type", string(res.TurnType),
			"matched_model", res.Decision.Model,
			"substitute_model", model,
		)
		return
	}
	// Retarget a copy of the routed decision, keeping its routing metadata;
	// the plan resolver authorizes the new target under the deployment
	// override source. The original's effort is the replaced model's knob.
	substitute := res.Decision
	substitute.Provider, substitute.Model = provider, model
	substitute.Effort = ""
	substitute.Reason = reason
	res.SubstitutedFrom = original
	res.SubstitutionReason = reason
	res.Decision = substitute
	res.Origin = policy.OverrideSourceDeployment
	observability.FromContext(ctx).Info("Local substitute served turn",
		"substitution_reason", reason,
		"turn_type", string(res.TurnType),
		"original_model", original.Model,
		"original_provider", original.Provider,
		"original_reason", original.Reason,
		"mapped_model", res.MappedDecision.Model,
		"substitute_model", model,
		"substitute_provider", provider,
	)
}
