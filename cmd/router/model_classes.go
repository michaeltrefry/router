package main

import (
	"errors"
	"fmt"

	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"
)

var (
	errModelClassesModel     = errors.New("model classes: entry must name a catalog or configured local model")
	errModelClassesTier      = errors.New("model classes: entry's tier differs from its class")
	errModelClassesDuplicate = errors.New("model classes: model listed twice")
)

// localModelClassesEntry orders, per class, the models x-weave-model-class
// requests fall back through.
type localModelClassesEntry struct {
	High []string `yaml:"high"`
	Mid  []string `yaml:"mid"`
	Low  []string `yaml:"low"`
}

// validateModelClasses resolves the model_classes block against the catalog
// as this deployment tiers it, so it must run after local models are
// registered and model_tiers applied. Every entry must carry its class's tier.
func validateModelClasses(entry *localModelClassesEntry) (proxy.ModelClassOrder, error) {
	if entry == nil {
		return nil, nil
	}
	out := proxy.ModelClassOrder{}
	seen := map[string]struct{}{}
	for _, class := range []struct {
		tier   catalog.Tier
		models []string
	}{{catalog.TierHigh, entry.High}, {catalog.TierMid, entry.Mid}, {catalog.TierLow, entry.Low}} {
		for _, model := range class.models {
			if _, known := catalog.ByID(model); !known {
				return nil, fmt.Errorf("%w: %q", errModelClassesModel, model)
			}
			if tier := catalog.TierFor(model); tier != class.tier {
				return nil, fmt.Errorf("%w: %q is %s, listed under %s", errModelClassesTier, model, tier, class.tier)
			}
			if _, dup := seen[model]; dup {
				return nil, fmt.Errorf("%w: %q", errModelClassesDuplicate, model)
			}
			seen[model] = struct{}{}
			out[class.tier] = append(out[class.tier], model)
		}
	}
	return out, nil
}
