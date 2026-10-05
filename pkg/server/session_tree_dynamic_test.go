package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionTreeTransportLateSnapshotSeedsBeforeLive(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		name := "compatibility"
		if ordered {
			name = "ordered"
		}
		t.Run(name, func(t *testing.T) {
			updates := make(chan runtime.TreeUpdate, 2)
			child := runtime.SessionSnapshot{Session: session.New(session.WithID("child"), session.WithParentID("root")), Status: runtime.SessionStatus{SessionID: "child", State: runtime.SessionStateRunning}, Cursor: 9}
			seedContent := "uncommitted"
			if ordered {
				seedContent = strings.Repeat("uncommitted", 10000)
			}
			seed := runtime.SessionEvent{Version: 2, SessionID: "child", TranscriptPosition: -1, Event: runtime.AgentChoice("worker", "child", seedContent)}
			tail := runtime.SessionEvent{Version: 2, SessionID: "child", Sequence: 10, Event: runtime.AgentChoice("worker", "child", " live")}
			updates <- runtime.TreeUpdate{Snapshot: &child, Replay: []runtime.SessionEvent{seed}}
			updates <- runtime.TreeUpdate{Event: &tail}
			handle := &httpSession{id: "root", agent: "root", attach: runtime.Observation{
				Initial:     []runtime.SessionSnapshot{{Session: session.New(session.WithID("root")), Status: runtime.SessionStatus{SessionID: "root"}}},
				TreeUpdates: updates,
			}}
			srv, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{"root": handle}})
			httpServer := httptest.NewServer(srv.e)
			defer httpServer.Close()
			client, err := runtime.NewClient(httpServer.URL, runtime.WithHTTPClient(httpServer.Client()))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client)
			require.NoError(t, err)
			remote, err := transport.SessionByID("root")
			require.NoError(t, err)
			observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{Tree: true, OrderedTree: ordered, Buffer: 1})
			require.NoError(t, err)
			defer observation.Cancel()
			handle.mu.Lock()
			assert.True(t, handle.attachOptions[0].OrderedTree, "server must consume one ordered tree authority")
			handle.mu.Unlock()
			if ordered {
				select {
				case baseline := <-observation.TreeUpdates:
					require.NotNil(t, baseline.Snapshot)
					assert.Equal(t, "child", baseline.Snapshot.Session.ID)
					require.Len(t, baseline.Replay, 1)
					assert.True(t, baseline.Replay[0].IsLiveSeed())
					assert.Equal(t, seedContent, baseline.Replay[0].Event.(*runtime.AgentChoiceEvent).Content)
				case <-time.After(time.Second):
					t.Fatal("missing dynamic baseline")
				}
				select {
				case live := <-observation.TreeUpdates:
					require.NotNil(t, live.Event)
					assert.Equal(t, uint64(10), live.Event.Sequence)
				case <-time.After(time.Second):
					t.Fatal("missing live event")
				}
			} else {
				select {
				case baseline := <-observation.SessionsAdded:
					assert.Equal(t, "child", baseline.Session.ID)
				case <-time.After(time.Second):
					t.Fatal("missing legacy snapshot")
				}
				select {
				case event := <-observation.Events:
					assert.True(t, event.IsLiveSeed())
				case <-time.After(time.Second):
					t.Fatal("missing legacy seed")
				}
				select {
				case event := <-observation.Events:
					assert.Equal(t, uint64(10), event.Sequence)
				case <-time.After(time.Second):
					t.Fatal("missing legacy live event")
				}
			}
		})
	}
}

func TestSessionTreeTransportCanonicalDynamicNestedChildren(t *testing.T) {
	model := sessionHTTPProvider{}
	srv, owner := newCanonicalLocalServer(t, session.NewInMemorySessionStore(),
		agent.New("root", "prompt", agent.WithModel(model), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(model), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	)
	root, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("root")), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	server := httptest.NewServer(srv.e)
	defer server.Close()
	client, err := runtime.NewClient(server.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(root.ID())
	require.NoError(t, err)
	observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{Tree: true, OrderedTree: true})
	require.NoError(t, err)
	defer observation.Cancel()
	for _, member := range []struct{ id, parent string }{{"child", "root"}, {"grandchild", "child"}} {
		_, err = owner.Runtime().CreateSession(t.Context(), session.New(session.WithID(member.id)), runtime.SessionBinding{AgentName: "worker", ParentSessionID: member.parent})
		require.NoError(t, err)
		deadline := time.After(3 * time.Second)
		for {
			select {
			case update, open := <-observation.TreeUpdates:
				require.True(t, open, "canonical tree observation unexpectedly closed")
				if update.Snapshot == nil {
					continue
				}
				require.Equal(t, member.id, update.Snapshot.Session.ID)
				require.Equal(t, member.parent, update.Snapshot.Session.ParentID)
			case err := <-observation.Errors:
				t.Fatalf("canonical tree observation failed: %v", err)
			case <-deadline:
				t.Fatal("canonical child baseline did not arrive")
			}
			break
		}
	}
}
