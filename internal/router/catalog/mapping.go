package catalog

import (
	"fmt"
	"maps"
	"slices"
)

// mappedSourceIDs tracks rows TierMappingSources tiered, so
// UntierMappingSources can never untier a statically tiered row.
var mappedSourceIDs = map[string]struct{}{}

// TierMappingSources gives each untiered mapping source the tier of the model
// that serves it, so a retired roster model whose selections are served on its
// family's current release is an automatic routing target again. A tiered
// source keeps its own tier; a source whose target is untiered stays
// passthrough-only. Boot-time only, like RegisterLocalModels; mutates nothing
// on error.
func TierMappingSources(mapping map[string]string) error {
	tiers := make(map[string]Tier, len(mapping))
	for _, source := range slices.Sorted(maps.Keys(mapping)) {
		src, ok := byID[source]
		if !ok {
			return fmt.Errorf("catalog: mapping source %q is not a catalog model", source)
		}
		target, ok := byID[mapping[source]]
		if !ok {
			return fmt.Errorf("catalog: mapping target %q is not a catalog model", mapping[source])
		}
		if src.Tier == TierUnknown && target.Tier != TierUnknown {
			tiers[source] = target.Tier
		}
	}
	for source, tier := range tiers {
		setTier(source, tier)
		mappedSourceIDs[source] = struct{}{}
	}
	return nil
}

// UntierMappingSources reverts rows tiered by TierMappingSources; other IDs
// are ignored. Same boot-time-only constraint as tiering.
func UntierMappingSources(ids ...string) {
	for _, id := range ids {
		if _, mapped := mappedSourceIDs[id]; !mapped {
			continue
		}
		delete(mappedSourceIDs, id)
		setTier(id, TierUnknown)
	}
}

func setTier(id string, tier Tier) {
	for i := range Models {
		if Models[i].ID == id {
			Models[i].Tier = tier
			byID[id] = Models[i]
			return
		}
	}
}
