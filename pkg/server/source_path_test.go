package server

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSourceRoutesPreserveExactEscapedIdentity(t *testing.T) {
	// Configuration parsing/provider construction is offline; no turns or tools
	// are executed. A placeholder credential avoids environment dependence.
	t.Setenv("OPENAI_API_KEY", "offline-unused")
	sources := config.Sources{}
	names := []string{"exact/team.yaml", "literal%2Fteam.yaml", "literal%team.yaml", "exact/literal%2Fteam.yaml"}
	for _, name := range names {
		data := "version: \"2\"\nagents:\n  root:\n    instruction: hi\n    description: \"" + name + "\"\n    model: openai/gpt-4o\n"
		sources[name] = config.NewBytesSource(name, []byte(data))
	}
	sm := NewSessionManager(t.Context(), sources, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})
	srv := httptest.NewServer(NewWithManager(sm, "").e)
	defer srv.Close()
	client, err := runtime.NewClient(srv.URL)
	require.NoError(t, err)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			cfg, err := client.GetAgent(t.Context(), name)
			require.NoError(t, err)
			require.Equal(t, name, cfg.Agents[0].Description)
			_, err = client.GetAgentToolCount(t.Context(), name, "root")
			require.NoError(t, err)
		})
	}
}
