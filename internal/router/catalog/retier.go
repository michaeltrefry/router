package catalog

import (
	"fmt"
	"maps"
	"slices"
)

// retieredFrom holds the static tier of each row RetierModels changed, so
// RestoreTiers can put it back.
var retieredFrom = map[string]Tier{}

// RetierModels assigns deployment-configured tiers to built-in rows that
// already carry one. Local and untiered rows are rejected: a local model
// declares its own tier, and tiering a passthrough-only row would make it an
// automatic routing target. Boot-time only, like RegisterLocalModels; mutates
// nothing on error.
func RetierModels(tiers map[string]Tier) error {
	for _, id := range slices.Sorted(maps.Keys(tiers)) {
		m, ok := byID[id]
		switch {
		case !ok:
			return fmt.Errorf("catalog: retier %q: not a catalog model", id)
		case IsLocal(id):
			return fmt.Errorf("catalog: retier %q: local models declare their own tier", id)
		case m.Tier == TierUnknown:
			return fmt.Errorf("catalog: retier %q: model is passthrough-only", id)
		case tiers[id] == TierUnknown:
			return fmt.Errorf("catalog: retier %q: target tier is unknown", id)
		}
	}
	for id, tier := range tiers {
		if _, saved := retieredFrom[id]; !saved {
			retieredFrom[id] = byID[id].Tier
		}
		setTier(id, tier)
	}
	return nil
}

// RestoreTiers reverts every row RetierModels changed to its static tier.
func RestoreTiers() {
	for id, tier := range retieredFrom {
		setTier(id, tier)
	}
	clear(retieredFrom)
}
