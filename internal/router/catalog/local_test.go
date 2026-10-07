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

func TestUnregisterLocalModels_RemovesOnlyLocalRows(t *testing.T) {
	before := len(catalog.Models)
	require.NoError(t, catalog.RegisterLocalModels(catalog.Model{ID: "local-unreg", Providers: []catalog.ProviderBinding{{Provider: "local_x"}}}))
	_, found := catalog.ByID("local-unreg")
	require.True(t, found)
	assert.True(t, catalog.IsLocal("local-unreg"))
	assert.False(t, catalog.IsLocal("claude-sonnet-4-6"), "a static row is never local")

	catalog.UnregisterLocalModels("local-unreg", "claude-sonnet-4-6")
	assert.False(t, catalog.IsLocal("local-unreg"), "an unregistered row stops being local")

	_, found = catalog.ByID("local-unreg")
	assert.False(t, found, "the local row leaves the ID index")
	assert.Len(t, catalog.Models, before, "the local row leaves Models")
	_, found = catalog.ByID("claude-sonnet-4-6")
	assert.True(t, found, "a static row is never removed")
}
