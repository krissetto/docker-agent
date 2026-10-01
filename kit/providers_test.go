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
			Name         string `yaml:"name"`
			ProxyManaged bool   `yaml:"proxyManaged"`
			Inject       []struct {
				Domain, Header, Format string
				Fields                 map[string]any `yaml:",inline"`
			} `yaml:"inject"`
			Fields map[string]any `yaml:",inline"`
		} `yaml:"apiKey"`
		Runtime struct {
			Allow  []string       `yaml:"allow"`
			Fields map[string]any `yaml:",inline"`
		} `yaml:"runtime"`
		Fields map[string]any `yaml:",inline"`
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

func TestKitCredentialPolicy(t *testing.T) {
	expected := map[string]struct {
		env, header, format string
		hosts               []string
		optional            bool
	}{
		"openai":    {"OPENAI_API_KEY", "Authorization", "Bearer %s", []string{"api.openai.com"}, false},
		"anthropic": {"ANTHROPIC_API_KEY", "x-api-key", "%s", []string{"api.anthropic.com"}, false},
		"google":    {"GOOGLE_API_KEY", "x-goog-api-key", "%s", []string{"generativelanguage.googleapis.com"}, true},
		"github":    {"GH_TOKEN", "Authorization", "Bearer %s", []string{"api.github.com", "github.com"}, true},
	}
	seen := map[string]bool{}
	var allowed, injected []string
	networkPolicies := 0
	for _, cap := range readKitCapabilities(t, "async-agent.yaml") {
		switch cap.Type {
		case "com.docker.sandbox/network-policy@1":
			networkPolicies++
			assert.False(t, cap.Optional)
			assert.ElementsMatch(t, []string{"runtime"}, slices.Collect(maps.Keys(cap.Config.Fields)), "no install routes or proxy bypass")
			assert.ElementsMatch(t, []string{"allow"}, slices.Collect(maps.Keys(cap.Config.Runtime.Fields)), "only the reviewed allowlist")
			allowed = append(allowed, cap.Config.Runtime.Allow...)
		case "com.docker.sandbox/credential@1":
			want, ok := expected[cap.Config.Service]
			require.True(t, ok, "kit may only expose the four reviewed services")
			require.NotContains(t, seen, cap.Config.Service)
			seen[cap.Config.Service] = true
			assert.Equal(t, want.optional, cap.Optional, cap.Config.Service)
			assert.True(t, cap.Config.APIKey.ProxyManaged)
			assert.Equal(t, "runtime", cap.Config.Phase)
			assert.Equal(t, want.env, cap.Config.APIKey.Name)
			assert.ElementsMatch(t, []string{"service", "phase", "apiKey"}, slices.Collect(maps.Keys(cap.Config.Fields)), "no OAuth or guest credential files")
			assert.ElementsMatch(t, []string{"name", "proxyManaged", "inject"}, slices.Collect(maps.Keys(cap.Config.APIKey.Fields)))
			var hosts []string
			for _, inject := range cap.Config.APIKey.Inject {
				assert.ElementsMatch(t, []string{"domain", "header", "format"}, slices.Collect(maps.Keys(inject.Fields)))
				assert.Equal(t, want.header, inject.Header)
				assert.Equal(t, want.format, inject.Format)
				hosts = append(hosts, inject.Domain)
			}
			assert.ElementsMatch(t, want.hosts, hosts, "exact injection hosts only")
			injected = append(injected, hosts...)
		}
	}
	assert.Equal(t, 1, networkPolicies)
	assert.ElementsMatch(t, slices.Collect(maps.Keys(expected)), slices.Collect(maps.Keys(seen)))
	assert.ElementsMatch(t, []string{"api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com", "api.github.com", "github.com"}, allowed)
	assert.ElementsMatch(t, allowed, injected, "no extra egress or secret-injection scopes")
}
