package providers_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
)

// registerLocalProvider registers name and removes it from the global provider
// maps when the test ends, so repeated runs and later tests see no leak.
func registerLocalProvider(t *testing.T, name, keyEnv string) error {
	t.Helper()
	err := providers.RegisterLocalProvider(name, keyEnv)
	if err == nil {
		t.Cleanup(func() {
			delete(providers.ProviderFamilies, name)
			delete(providers.APIKeyEnvVars, name)
		})
	}
	return err
}

func TestRegisterLocalProvider_DispatchesAsOpenAICompat(t *testing.T) {
	name := providers.LocalProviderName("providers-test-model")

	require.NoError(t, registerLocalProvider(t, name, "PROVIDERS_TEST_KEY"))

	assert.Equal(t, providers.FamilyOpenAICompat, providers.FamilyFor(name))
	assert.Equal(t, "PROVIDERS_TEST_KEY", providers.APIKeyEnvVar(name))
	assert.False(t, providers.IsGateway(name), "a local model must never become gateway-exclusive")
	require.NoError(t, providers.ValidateDispatchable([]string{name}))
}

func TestRegisterLocalProvider_RejectsCollisions(t *testing.T) {
	name := providers.LocalProviderName("providers-test-dup")
	require.NoError(t, registerLocalProvider(t, name, "KEY"))

	require.ErrorIs(t, registerLocalProvider(t, name, "KEY"), providers.ErrProviderAlreadyRegistered)
	assert.Error(t, registerLocalProvider(t, providers.ProviderOpenAI, "KEY"), "built-in names are not local")
}
