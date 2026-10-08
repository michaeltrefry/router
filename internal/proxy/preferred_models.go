package proxy

import (
	"context"
	"path"
	"slices"
)

// expandPreferredModels follows each preferred model with the routable models
// the deployment serves as it — model-mapping sources and models matched by a
// substitution rule onto a local model — at the same position in the ranking.
// The scorer only ranks the models it selects, so a preferred mapping target
// or local model would otherwise be a silent no-op.
func (s *Service) expandPreferredModels(ctx context.Context, preferred []string) []string {
	rules := s.substitutionRules
	if localRoutingDisabled(ctx) {
		rules = nil
	}
	if len(preferred) == 0 || (len(s.modelMapping) == 0 && len(rules) == 0) {
		return preferred
	}
	universe := s.routableUniverse()
	ids := make([]string, 0, len(universe))
	for id := range universe {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	out := make([]string, 0, len(preferred))
	for _, want := range preferred {
		out = append(out, want)
		for _, id := range ids {
			if id != want && servedAs(id, s.modelMapping, rules) == want {
				out = append(out, id)
			}
		}
	}
	return out
}

// servedAs is the model an automatic selection of id is served on when every
// mapping and substitution rule applies.
func servedAs(id string, mapping ModelMapping, rules []SubstitutionRule) string {
	served := id
	if target, ok := mapping[id]; ok {
		served = target
	}
	for _, rule := range rules {
		if matched, _ := path.Match(rule.Match, served); matched {
			return rule.Model
		}
	}
	return served
}
