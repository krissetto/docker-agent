//go:build js && wasm && !docker_agent_wasm_cloud

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestWasmGoogleFactoryRejectsVertexWithoutCloudBuild(t *testing.T) {
	t.Parallel()
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})
	for _, opts := range []map[string]any{{"project": "test-project"}, {"publisher": "anthropic"}} {
		cfg := &latest.ModelConfig{Provider: "google", Model: "test-model", ProviderOpts: opts}
		_, err := demoProviders.New(t.Context(), cfg, env)
		require.ErrorContains(t, err, "requires a build with -tags docker_agent_wasm_cloud")
	}
}
