package providers

import (
	"errors"
	"fmt"
	"strings"
)

// LocalProviderPrefix prefixes every provider name minted for a self-hosted
// model declared in deployment configuration; each local model gets its own
// provider so several local servers can coexist.
const LocalProviderPrefix = "local_"

// ErrProviderAlreadyRegistered is returned when a local provider name collides
// with a built-in or previously registered provider.
var ErrProviderAlreadyRegistered = errors.New("provider already registered")

// LocalProviderName returns the provider name serving the local model id.
func LocalProviderName(modelID string) string {
	return LocalProviderPrefix + modelID
}

// IsLocalProvider reports whether name is a provider minted for a self-hosted
// model.
func IsLocalProvider(name string) bool {
	return strings.HasPrefix(name, LocalProviderPrefix)
}

// RegisterLocalProvider adds an OpenAI-compatible local provider to the
// provider maps. Boot-time only: the maps are read without locking once the
// server is serving.
func RegisterLocalProvider(name, apiKeyEnvVar string) error {
	if !strings.HasPrefix(name, LocalProviderPrefix) {
		return fmt.Errorf("local provider %q must start with %q", name, LocalProviderPrefix)
	}
	if apiKeyEnvVar == "" {
		return fmt.Errorf("local provider %q has no API-key env var", name)
	}
	if _, exists := ProviderFamilies[name]; exists {
		return fmt.Errorf("%w: %s", ErrProviderAlreadyRegistered, name)
	}
	ProviderFamilies[name] = FamilyOpenAICompat
	APIKeyEnvVars[name] = apiKeyEnvVar
	return nil
}
