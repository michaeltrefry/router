package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/router/catalog"
)

// Only rejections are exercised here: a successful registration would leak a
// $0 row into the static-catalog invariants this package's tests assert.
func TestRegisterLocalModels_RejectsInvalidRowsWithoutMutating(t *testing.T) {
	binding := []catalog.ProviderBinding{{Provider: "local_x"}}
	before := len(catalog.Models)

	require.ErrorIs(t, catalog.RegisterLocalModels(catalog.Model{ID: "claude-sonnet-4-6", Providers: binding}), catalog.ErrDuplicateModelID)
	require.ErrorIs(t, catalog.RegisterLocalModels(
		catalog.Model{ID: "local-a", Providers: binding},
		catalog.Model{ID: "local-a", Providers: binding},
	), catalog.ErrDuplicateModelID)
	require.Error(t, catalog.RegisterLocalModels(catalog.Model{ID: "local-b"}))
	require.Error(t, catalog.RegisterLocalModels(catalog.Model{Providers: binding}))

	assert.Len(t, catalog.Models, before)
	_, found := catalog.ByID("local-a")
	assert.False(t, found, "a rejected batch registers nothing")
}
