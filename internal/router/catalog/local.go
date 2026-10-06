package catalog

import (
	"errors"
	"fmt"
)

// ErrDuplicateModelID is returned when a registered model reuses a catalog ID.
var ErrDuplicateModelID = errors.New("catalog: duplicate model id")

// localIDs tracks rows added by RegisterLocalModels so UnregisterLocalModels
// can never remove a static catalog row.
var localIDs = map[string]struct{}{}

// RegisterLocalModels appends deployment-configured self-hosted models to the
// catalog. The caller parses configuration; this package stays I/O-free.
// Boot-time only: Models and the ID index are read without locking once the
// server is serving.
func RegisterLocalModels(models ...Model) error {
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m.ID == "" {
			return errors.New("catalog: local model has no id")
		}
		if _, exists := byID[m.ID]; exists {
			return fmt.Errorf("%w: %s", ErrDuplicateModelID, m.ID)
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateModelID, m.ID)
		}
		if len(m.Providers) == 0 {
			return fmt.Errorf("catalog: local model %s has no provider binding", m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	for _, m := range models {
		Models = append(Models, m)
		byID[m.ID] = m
		localIDs[m.ID] = struct{}{}
	}
	return nil
}

// IsLocal reports whether id was registered by RegisterLocalModels.
func IsLocal(id string) bool {
	_, local := localIDs[id]
	return local
}

// UnregisterLocalModels removes rows previously added by RegisterLocalModels;
// other IDs are ignored. Same boot-time-only constraint as registration.
func UnregisterLocalModels(ids ...string) {
	for _, id := range ids {
		if _, local := localIDs[id]; !local {
			continue
		}
		delete(localIDs, id)
		delete(byID, id)
		for i, m := range Models {
			if m.ID == id {
				Models = append(Models[:i:i], Models[i+1:]...)
				break
			}
		}
	}
}
