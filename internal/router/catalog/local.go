package catalog

import (
	"errors"
	"fmt"
)

// ErrDuplicateModelID is returned when a registered model reuses a catalog ID.
var ErrDuplicateModelID = errors.New("catalog: duplicate model id")

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
	}
	return nil
}
