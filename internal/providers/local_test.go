package providers_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
)

func TestRegisterLocalProvider_DispatchesAsOpenAICompat(t *testing.T) {
	name := providers.LocalProviderName("providers-test-model")

	require.NoError(t, providers.RegisterLocalProvider(name, "PROVIDERS_TEST_KEY"))

	assert.Equal(t, providers.FamilyOpenAICompat, providers.FamilyFor(name))
	assert.Equal(t, "PROVIDERS_TEST_KEY", providers.APIKeyEnvVar(name))
	assert.False(t, providers.IsGateway(name), "a local model must never become gateway-exclusive")
	require.NoError(t, providers.ValidateDispatchable([]string{name}))
}

func TestRegisterLocalProvider_RejectsCollisions(t *testing.T) {
	name := providers.LocalProviderName("providers-test-dup")
	require.NoError(t, providers.RegisterLocalProvider(name, "KEY"))

	require.ErrorIs(t, providers.RegisterLocalProvider(name, "KEY"), providers.ErrProviderAlreadyRegistered)
	assert.Error(t, providers.RegisterLocalProvider(providers.ProviderOpenAI, "KEY"), "built-in names are not local")
}
