package asyncsubagents_test

import (
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type kitCapability struct {
	Type     string `yaml:"type"`
	Optional bool   `yaml:"optional"`
	Config   struct {
		Service string `yaml:"service"`
		Phase   string `yaml:"phase"`
		APIKey  struct {
			Name         string                                    `yaml:"name"`
			ProxyManaged bool                                      `yaml:"proxyManaged"`
			Inject       []struct{ Domain, Header, Format string } `yaml:"inject"`
		} `yaml:"apiKey"`
		Runtime struct {
			Allow []string `yaml:"allow"`
		} `yaml:"runtime"`
	} `yaml:"config"`
}

func readKitCapabilities(t *testing.T, path string) []kitCapability {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var d struct {
		Capabilities []kitCapability `yaml:"capabilities"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &d))
	return d.Capabilities
}
func TestKitOnlyDeclaresThreeOptionalProviderCredentials(t *testing.T) {
	expected := map[string]struct{ env, host, header, format string }{
		"openai":    {"OPENAI_API_KEY", "api.openai.com", "Authorization", "Bearer %s"},
		"anthropic": {"ANTHROPIC_API_KEY", "api.anthropic.com", "x-api-key", "%s"},
		"google":    {"GOOGLE_API_KEY", "generativelanguage.googleapis.com", "x-goog-api-key", "%s"},
	}
	seen := map[string]bool{}
	var allowed, injected []string
	for _, cap := range readKitCapabilities(t, "async-agent.yaml") {
		switch cap.Type {
		case "com.docker.sandbox/network-policy@1":
			allowed = append(allowed, cap.Config.Runtime.Allow...)
		case "com.docker.sandbox/credential@1":
			want, ok := expected[cap.Config.Service]
			require.True(t, ok, "kit may only expose the three requested providers")
			require.NotContains(t, seen, cap.Config.Service)
			seen[cap.Config.Service] = true
			assert.True(t, cap.Optional)
			assert.True(t, cap.Config.APIKey.ProxyManaged)
			assert.Equal(t, "runtime", cap.Config.Phase)
			assert.Equal(t, want.env, cap.Config.APIKey.Name)
			require.Len(t, cap.Config.APIKey.Inject, 1)
			inject := cap.Config.APIKey.Inject[0]
			assert.Equal(t, want.host, inject.Domain)
			assert.Equal(t, want.header, inject.Header)
			assert.Equal(t, want.format, inject.Format)
			injected = append(injected, inject.Domain)
		}
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(expected)), slices.Collect(maps.Keys(seen)))
	assert.ElementsMatch(t, []string{"api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com"}, allowed)
	assert.ElementsMatch(t, allowed, injected, "no extra egress or secret-injection scopes")
}
