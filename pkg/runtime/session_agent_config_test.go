package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type privateInspectorToolset struct{ toolListToolset }

func (*privateInspectorToolset) Kind() string { return "Remote MCP" }

func TestSessionAgentConfigLocalRemoteParity(t *testing.T) {
	child := agent.New("child", "child-private-instruction", agent.WithModel(&mockProvider{id: "test/child"}))
	root := agent.New("root", "private-instruction", agent.WithModel(&mockProvider{id: "test/root"}), agent.WithFallbackModel(&mockProvider{id: "test/fallback"}), agent.WithSubAgents(child), agent.WithMaxIterations(12), agent.WithNumHistoryItems(5), agent.WithAddDate(true), agent.WithToolSets(&privateInspectorToolset{toolListToolset{desc: "mcp(remote host=private.internal)", names: []string{"lookup"}}}))
	cfg := latest.AgentConfig{Skills: latest.SkillsConfig{Include: []string{"review"}}, Toolsets: []latest.Toolset{{Type: "mcp", Command: "/private/server", Env: map[string]string{"TOKEN": "private-token"}, Headers: map[string]string{"Authorization": "private-auth"}, Tools: []string{"lookup"}}}}
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, child), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": cfg})), WithCurrentAgent("child"), WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	sess := session.New(session.WithID("bound"))
	sess.SetAttribute("docker-agent.actor.source", "team.yaml")
	h, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	local, err := h.(SessionAgentConfigReader).SessionAgentConfig(t.Context(), "root")
	require.NoError(t, err)
	require.True(t, local.Info.IsCurrent, "current means the bound session, not global router")
	require.Equal(t, []string{"child"}, local.Info.SubAgents)
	require.Equal(t, []string{"test/fallback"}, local.Info.Fallbacks)
	require.Equal(t, []string{"review"}, local.Info.Skills)
	require.Equal(t, 12, local.Info.MaxIterations)
	require.Equal(t, "mcp", local.Info.Toolsets[0].Name)
	require.Equal(t, ToolsetStopped, local.Info.Toolsets[0].State)
	data, err := json.Marshal(local)
	require.NoError(t, err)
	for _, secret := range []string{"private-instruction", "private-token", "private-auth", "private.internal", "/private/server"} {
		require.NotContains(t, string(data), secret)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/api/v2/sessions/bound/agent-config", req.URL.Path)
		out, readErr := h.(SessionAgentConfigReader).SessionAgentConfig(req.Context(), req.URL.Query().Get("agent"))
		if !assert.NoError(t, readErr) {
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(out))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
	require.NoError(t, err)
	remote, err := transport.SessionByID("bound")
	require.NoError(t, err)
	got, err := remote.(SessionAgentConfigReader).SessionAgentConfig(t.Context(), "root")
	require.NoError(t, err)
	require.Equal(t, local, got)
	_, err = h.(SessionAgentConfigReader).SessionAgentConfig(t.Context(), "foreign")
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.(SessionAgentConfigReader).SessionAgentConfig(ctx, "root")
	require.ErrorIs(t, err, context.Canceled)
	started, _ := root.ToolSets()[0].(*tools.StartableToolSet).TryState()
	require.False(t, started, "inspection never starts a toolset")
}

func TestSessionAgentConfigRejectsForeignIdentity(t *testing.T) {
	for _, response := range []SessionAgentConfig{
		{SessionID: "foreign", Source: "team.yaml", AgentName: "root"},
		{SessionID: "bound", Source: "foreign.yaml", AgentName: "root"},
		{SessionID: "bound", Source: "team.yaml", AgentName: "foreign"},
	} {
		t.Run(response.SessionID+response.Source+response.AgentName, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { assert.NoError(t, json.NewEncoder(w).Encode(response)) }))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client, WithSessionTransportSource("team.yaml"))
			require.NoError(t, err)
			h, err := transport.SessionByID("bound")
			require.NoError(t, err)
			got, err := h.(SessionAgentConfigReader).SessionAgentConfig(t.Context(), "root")
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}
