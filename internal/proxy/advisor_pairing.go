package proxy

import (
	"sort"
	"strings"

	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/translate"
)

// excludeAdvisorOutrankingModels augments excluded with every Claude model
// the request's advisor_* tool cannot advise: Anthropic 400s an advisor ranked
// below the request model, and the client validated the pairing only against
// the model it asked for. Claude models with no advisor rank are dropped too.
// Returns every such model in available, including ones already excluded, so
// callers can keep them out of later re-admission.
func excludeAdvisorOutrankingModels(env *translate.RequestEnvelope, excluded, available map[string]struct{}) (map[string]struct{}, []string) {
	advisorRank, ok := catalog.AdvisorRankFor(env.AdvisorToolModel())
	if !ok {
		return excluded, nil
	}
	var unadvisableModels []string
	for model := range available {
		if !strings.HasPrefix(model, "claude-") {
			continue
		}
		if rank, ranked := catalog.AdvisorRankFor(model); !ranked || rank > advisorRank {
			unadvisableModels = append(unadvisableModels, model)
		}
	}
	if len(unadvisableModels) == 0 {
		return excluded, nil
	}
	updatedExcluded := make(map[string]struct{}, len(excluded)+len(unadvisableModels))
	for model := range excluded {
		updatedExcluded[model] = struct{}{}
	}
	for _, model := range unadvisableModels {
		updatedExcluded[model] = struct{}{}
	}
	sort.Strings(unadvisableModels)
	return updatedExcluded, unadvisableModels
}
