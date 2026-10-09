package main

import (
	"errors"
	"fmt"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router/catalog"
)

var (
	errLowTierSubstituteModel     = errors.New("low-tier substitute: model must name a configured local model")
	errLowTierSubstituteTier      = errors.New("low-tier substitute: model must be tier low")
	errLowTierSubstituteDuplicate = errors.New("low-tier substitute: model listed twice")
	errLowTierSubstituteEmpty     = errors.New("low-tier substitute: models is required")
)

// localLowTierSubstituteEntry lists, in order, the local models that replace
// automatic low-tier selections. Enabled defaults to true.
type localLowTierSubstituteEntry struct {
	Models  []string `yaml:"models"`
	Enabled *bool    `yaml:"enabled"`
}

// validateLowTierSubstitute resolves the low_tier_substitute block. Every
// model must be a configured tier-low local model, so a low-tier pick is never
// served by a stronger or weaker class.
func validateLowTierSubstitute(entry *localLowTierSubstituteEntry, tiers map[string]catalog.Tier) (proxy.LowTierSubstitute, error) {
	if entry == nil {
		return proxy.LowTierSubstitute{}, nil
	}
	if len(entry.Models) == 0 {
		return proxy.LowTierSubstitute{}, errLowTierSubstituteEmpty
	}
	seen := make(map[string]struct{}, len(entry.Models))
	targets := make([]proxy.LocalTarget, 0, len(entry.Models))
	for _, model := range entry.Models {
		tier, configured := tiers[model]
		if !configured {
			return proxy.LowTierSubstitute{}, fmt.Errorf("%w: %q", errLowTierSubstituteModel, model)
		}
		if tier != catalog.TierLow {
			return proxy.LowTierSubstitute{}, fmt.Errorf("%w: %q is tier %s", errLowTierSubstituteTier, model, tier)
		}
		if _, dup := seen[model]; dup {
			return proxy.LowTierSubstitute{}, fmt.Errorf("%w: %q", errLowTierSubstituteDuplicate, model)
		}
		seen[model] = struct{}{}
		targets = append(targets, proxy.LocalTarget{Provider: providers.LocalProviderName(model), Model: model})
	}
	if entry.Enabled != nil && !*entry.Enabled {
		return proxy.LowTierSubstitute{}, nil
	}
	return proxy.LowTierSubstitute{Targets: targets}, nil
}
