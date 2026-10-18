package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCanonicalAwaitWorkflowReturnsSettledOutputFence(t *testing.T) {
	srv, owner := newCanonicalLocalServer(t, session.NewInMemorySessionStore(), agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	local, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("workflow")), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(local.ID())
	require.NoError(t, err)
	got, err := remote.(runtime.SessionWorkflow).AwaitWorkflow(t.Context())
	require.NoError(t, err)
	want, err := local.(runtime.SessionWorkflow).AwaitWorkflow(context.WithoutCancel(t.Context()))
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NotEmpty(t, got.Epoch)
}
