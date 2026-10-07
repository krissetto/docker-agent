package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/stretchr/testify/require"
)

func TestColdRootLookupRestoresDormantTreeAndTranscript(t *testing.T) {
	for _, entry := range []string{"http-lookup", "confirmed-view", "compatibility-loader"} {
		t.Run(entry, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "owned.db")
			store, err := sqlitestore.New(ctx, path)
			require.NoError(t, err)
			root := session.New(session.WithID("cold-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
			require.NoError(t, store.AddSession(ctx, root))
			child := session.New(session.WithID("cold-child"), session.WithParentID(root.ID), session.WithAgentName("worker"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker"}))
			rootID := subagent.SessionRootID(root.ID)
			node := subagent.Node{ID: "child-node", Parent: rootID, SessionID: child.ID, Agent: "worker", State: subagent.NodeIdle}
			require.NoError(t, store.(session.CoordinationStore).AdmitChild(ctx, session.ChildAdmission{Child: child, Record: session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: node}}))
			message := session.UserMessage("saved child transcript")
			message.Accepted, message.Pending, message.TurnID = true, true, "accepted-child"
			_, err = store.AddMessage(ctx, child.ID, message)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = sqlitestore.New(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			srv, owner := newCanonicalLocalServer(t, store,
				agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
				agent.New("worker", "prompt", agent.WithModel(sessionHTTPProvider{})),
			)
			httpServer := httptest.NewServer(srv.e)
			defer httpServer.Close()
			client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client)
			require.NoError(t, err)
			var snapshot *session.Session
			switch entry {
			case "confirmed-view":
				committed, err := runtime.RestoreSessionView(ctx, transport, root.ID)
				require.NoError(t, err)
				snapshot = committed.Info.Session
			case "compatibility-loader":
				_, snapshot, err = transport.LoadSession(ctx, root.ID)
				require.NoError(t, err)
			default:
				handle, err := transport.SessionByID(root.ID)
				require.NoError(t, err)
				require.NoError(t, handle.(runtime.Hydrator).Hydrate(ctx))
				snapshot, err = handle.Snapshot(ctx)
				require.NoError(t, err)
			}
			require.NotNil(t, snapshot.GetSubagentTree())
			require.Len(t, snapshot.GetSubagentTree().Nodes[0].Children, 1)
			childHandle, err := owner.Runtime().SessionByID(child.ID)
			require.NoError(t, err)
			childSnapshot, err := childHandle.Snapshot(ctx)
			require.NoError(t, err)
			require.Len(t, childSnapshot.MessagesSnapshot(), 1)
			status, err := childHandle.Status(ctx)
			require.NoError(t, err)
			require.True(t, status.Dormant)
			require.Equal(t, 1, status.Pending)
			require.Empty(t, status.TurnID)
			response := sessionRequest(t, srv, http.MethodGet, "/api/v2/sessions/"+child.ID+"/status", "", "")
			require.Equal(t, http.StatusOK, response.Code)
			tree, err := owner.Runtime().(runtime.TreeInspector).InspectSessionTree(ctx, root.ID)
			require.NoError(t, err)
			require.Len(t, tree.Nodes[0].Children, 1)
		})
	}
}

func TestColdRootLookupPreservesInterruptedOutcomeWarning(t *testing.T) {
	store := session.NewInMemorySessionStore()
	root := session.New(session.WithID(t.Name()), session.WithAgentName("root"), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	accepted := session.UserMessage("accepted before the previous process ended")
	accepted.Accepted, accepted.Pending, accepted.TurnID = true, false, "unfinished-turn"
	root.AddMessage(accepted)
	require.NoError(t, store.AddSession(t.Context(), root))
	srv, _ := newCanonicalLocalServer(t, store, agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{})))
	handle, err := srv.sm.Handle(t.Context(), root.ID)
	require.NoError(t, err)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Dormant)
	require.Equal(t, 1, status.InterruptedTurns)
	require.Equal(t, runtime.SessionStateSettled, status.State)
	require.ErrorContains(t, handle.AwaitTurn(t.Context(), "unfinished-turn"), "interrupted")
}
