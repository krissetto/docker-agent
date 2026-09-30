package asyncsubagents_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	openaiprovider "github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
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
func TestEveryEngineProviderHasExplicitKitCredentialPolicy(t *testing.T) {
	credentials := map[string]kitCapability{}
	var allowed []string
	for _, cap := range readKitCapabilities(t, "async-agent.yaml") {
		switch cap.Type {
		case "com.docker.sandbox/network-policy@1":
			allowed = append(allowed, cap.Config.Runtime.Allow...)
		case "com.docker.sandbox/credential@1":
			require.NotContains(t, credentials, cap.Config.Service)
			require.True(t, cap.Optional, "unbound services must not block unrelated providers")
			require.Equal(t, "runtime", cap.Config.Phase)
			require.True(t, cap.Config.APIKey.ProxyManaged)
			credentials[cap.Config.Service] = cap
		}
	}
	expected := map[string]struct {
		env, header, format string
		hosts               []string
	}{
		"openai":         {"OPENAI_API_KEY", "Authorization", "Bearer %s", []string{"api.openai.com"}},
		"anthropic":      {"ANTHROPIC_API_KEY", "x-api-key", "%s", []string{"api.anthropic.com"}},
		"google":         {"GOOGLE_API_KEY", "x-goog-api-key", "%s", []string{"generativelanguage.googleapis.com"}},
		"amazon-bedrock": {"AWS_BEARER_TOKEN_BEDROCK", "Authorization", "Bearer %s", []string{"bedrock-runtime.*.amazonaws.com", "bedrock-runtime.*.amazonaws.com.cn"}},
	}
	classified := map[string]string{"openai": "openai", "openai_chatcompletions": "openai", "openai_responses": "openai", "anthropic": "anthropic", "google": "google", "amazon-bedrock": "amazon-bedrock", "dmr": "local", "ollama": "local", "azure": "scoped-extension"}
	for name, alias := range provider.EachAlias() {
		if name == "azure" || name == "ollama" {
			continue
		}
		service := name
		if strings.HasPrefix(name, "cloudflare-") {
			service = "cloudflare"
		}
		if strings.HasPrefix(name, "opencode-") {
			service = "opencode"
		}
		endpoint, err := url.Parse(alias.BaseURL)
		require.NoError(t, err)
		entry := expected[service]
		entry.env = alias.TokenEnvVar
		entry.header = "Authorization"
		entry.format = "Bearer %s"
		if !slices.Contains(entry.hosts, endpoint.Hostname()) {
			entry.hosts = append(entry.hosts, endpoint.Hostname())
		}
		expected[service] = entry
		classified[name] = service
	}
	require.ElementsMatch(t, slices.Collect(maps.Keys(expected)), slices.Collect(maps.Keys(credentials)))
	seenEnv := map[string]string{}
	injected := map[string]bool{}
	for service, want := range expected {
		got := credentials[service]
		assert.Equal(t, want.env, got.Config.APIKey.Name, service)
		require.NotContains(t, seenEnv, want.env, "aliases using one environment name must share a service")
		seenEnv[want.env] = service
		var hosts []string
		for _, inject := range got.Config.APIKey.Inject {
			hosts = append(hosts, inject.Domain)
			injected[inject.Domain] = true
			assert.Equal(t, want.header, inject.Header, service)
			assert.Equal(t, want.format, inject.Format, service)
			if strings.Contains(inject.Domain, "*") {
				assert.Contains(t, []string{"bedrock-runtime.*.amazonaws.com", "bedrock-runtime.*.amazonaws.com.cn"}, inject.Domain)
			}
		}
		assert.ElementsMatch(t, want.hosts, hosts, service)
	}
	assert.ElementsMatch(t, allowed, slices.Collect(maps.Keys(injected)), "egress and injection must have exactly the same reviewed hosts")
	known := append(slices.Clone(provider.CoreProviders), slices.Collect(maps.Keys(providers.DefaultFactories()))...)
	for name := range provider.EachAlias() {
		known = append(known, name)
	}
	slices.Sort(known)
	known = slices.Compact(known)
	assert.ElementsMatch(t, known, slices.Collect(maps.Keys(classified)), "new engine providers require an explicit policy decision")
	readme, err := os.ReadFile("README.md")
	require.NoError(t, err)
	for _, name := range known {
		assert.Contains(t, string(readme), "`"+name+"`", "document every supported name")
	}
	azure := readKitCapabilities(t, "examples/azure.yaml")[1]
	assert.Equal(t, "AZURE_API_KEY", azure.Config.APIKey.Name)
	assert.Equal(t, "Authorization", azure.Config.APIKey.Inject[0].Header)
	assert.Equal(t, "Bearer %s", azure.Config.APIKey.Inject[0].Format, "engine Azure alias uses OpenAI Bearer, not an invented api-key header")
}

func TestOAuthAccessTokenAliasesAcceptProxySentinelsWithoutLogin(t *testing.T) {
	for _, tc := range []struct{ name, env string }{{"chatgpt", "CHATGPT_OAUTH_TOKEN"}, {"github-copilot", "GITHUB_TOKEN"}} {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := &latest.ModelConfig{Provider: tc.name, Model: "gpt-4o", BaseURL: server.URL, TokenKey: tc.env}
			if tc.name == "chatgpt" {
				cfg.ProviderOpts = map[string]any{"http_headers": map[string]any{"chatgpt-account-id": "nonsecret-account-id"}}
			}
			client, err := openaiprovider.NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{tc.env: "sbx-proxy-sentinel"}))
			require.NoError(t, err)
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "local test"}}, nil)
			require.NoError(t, err)
			for {
				if _, err := stream.Recv(); err != nil {
					break
				}
			}
			stream.Close()
			headers := <-captured
			assert.Equal(t, "Bearer sbx-proxy-sentinel", headers.Get("Authorization"))
			if tc.name == "chatgpt" {
				assert.Equal(t, "nonsecret-account-id", headers.Get("chatgpt-account-id"))
				assert.NotEmpty(t, headers.Get("OpenAI-Beta"))
			} else {
				assert.NotEmpty(t, headers.Get("Copilot-Integration-Id"))
			}
		})
	}
}
