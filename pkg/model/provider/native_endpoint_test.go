package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Native SDK providers must not inherit catalogue endpoint or token defaults,
// so their SDKs keep resolving endpoint environment overrides first.
func TestNativeProvidersReceiveNoCatalogEndpointDefaults(t *testing.T) {
	t.Parallel()

	for _, name := range CoreProviders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := applyProviderDefaults(&latest.ModelConfig{Provider: name, Model: "test-model"}, nil)
			assert.Empty(t, got.BaseURL, "catalogue endpoint must not shadow SDK environment overrides")
			assert.Empty(t, got.TokenKey, "catalogue token must not be applied to a native SDK provider")
		})
	}
}

// An explicit user endpoint on a native provider is kept verbatim.
func TestNativeProvidersKeepExplicitEndpoint(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"google", "anthropic", "openai"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := applyProviderDefaults(&latest.ModelConfig{Provider: name, Model: "test-model", BaseURL: "https://proxy.example/v1"}, nil)
			assert.Equal(t, "https://proxy.example/v1", got.BaseURL)
		})
	}
}
