package proxy

import (
	"sort"

	"weave-os/router/internal/router"
	"weave-os/router/internal/translate"
)

// excludeAdvisorOutrankingModels augments excluded with every Claude model
// that outranks the request's advisor_* tool model: Anthropic 400s an advisor
// ranked below the request model, and the client validated the pairing only
// against the model it asked for. Returns excluded unchanged (and nil) when
// nothing is added.
func excludeAdvisorOutrankingModels(env *translate.RequestEnvelope, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
	advisorRank, ok := router.AdvisorRank(env.AdvisorToolModel())
	if !ok {
		return excluded, nil
	}
	var out map[string]struct{}
	var added []string
	for model := range available {
		if rank, ranked := router.AdvisorRank(model); !ranked || rank <= advisorRank {
			continue
		}
		if _, already := excluded[model]; already {
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, len(excluded)+1)
			for k := range excluded {
				out[k] = struct{}{}
			}
		}
		out[model] = struct{}{}
		added = append(added, model)
	}
	if len(added) == 0 {
		return excluded, nil
	}
	sort.Strings(added)
	return out, added
}
