package main

import (
	"errors"
	"fmt"
	"path"

	"weave-os/router/internal/router/catalog"
)

var (
	errModelTiersGlob    = errors.New("model tiers: pattern is not a valid glob")
	errModelTiersNoMatch = errors.New("model tiers: pattern names no tiered catalog model")
	errModelTiersOverlap = errors.New("model tiers: model matches more than one tier")
)

// localModelTiersEntry retiers built-in catalog models: each list holds model
// IDs or glob patterns, and Default (low, mid or high) tiers every other
// tiered built-in model. Untiered passthrough-only rows are never tiered.
type localModelTiersEntry struct {
	High    []string `yaml:"high"`
	Mid     []string `yaml:"mid"`
	Low     []string `yaml:"low"`
	Default string   `yaml:"default"`
}

// validateModelTiers resolves the model_tiers block against the tiered
// built-in catalog rows. Every pattern must match at least one such row, so a
// misspelt ID cannot silently leave a model in its static tier.
func validateModelTiers(entry *localModelTiersEntry) (map[string]catalog.Tier, error) {
	if entry == nil {
		return nil, nil
	}
	fallback := catalog.TierUnknown
	if entry.Default != "" {
		tier, err := parseLocalTier(entry.Default)
		if err != nil {
			return nil, fmt.Errorf("model tiers: default: %w", err)
		}
		fallback = tier
	}
	classes := []struct {
		tier     catalog.Tier
		patterns []string
	}{{catalog.TierHigh, entry.High}, {catalog.TierMid, entry.Mid}, {catalog.TierLow, entry.Low}}
	for _, class := range classes {
		for _, pattern := range class.patterns {
			if _, err := path.Match(pattern, ""); err != nil {
				return nil, fmt.Errorf("%w: %q", errModelTiersGlob, pattern)
			}
			if !matchesTieredCatalogModel(pattern) {
				return nil, fmt.Errorf("%w: %q", errModelTiersNoMatch, pattern)
			}
		}
	}
	out := map[string]catalog.Tier{}
	for _, m := range catalog.Models {
		if m.Tier == catalog.TierUnknown || catalog.IsLocal(m.ID) {
			continue
		}
		assigned := catalog.TierUnknown
		for _, class := range classes {
			if !matchesAny(class.patterns, m.ID) {
				continue
			}
			if assigned != catalog.TierUnknown {
				return nil, fmt.Errorf("%w: %q is %s and %s", errModelTiersOverlap, m.ID, assigned, class.tier)
			}
			assigned = class.tier
		}
		if assigned == catalog.TierUnknown {
			assigned = fallback
		}
		if assigned != catalog.TierUnknown && assigned != m.Tier {
			out[m.ID] = assigned
		}
	}
	return out, nil
}

func matchesTieredCatalogModel(pattern string) bool {
	for _, m := range catalog.Models {
		if m.Tier == catalog.TierUnknown || catalog.IsLocal(m.ID) {
			continue
		}
		if matched, _ := path.Match(pattern, m.ID); matched {
			return true
		}
	}
	return false
}

func matchesAny(patterns []string, id string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, id); matched {
			return true
		}
	}
	return false
}
