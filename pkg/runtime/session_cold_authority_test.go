package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// coldAuthFixture: root with sync sub_agents librarian, plus an undeclared intruder.
type coldAuthFixture struct {
	root, librarian, intruder *agent.Agent
}

func newColdAuthFixture() coldAuthFixture {
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("transferred").AddStopWithUsage(10, 5).Build()}
	f := coldAuthFixture{
		librarian: agent.New("librarian", "Library agent", agent.WithModel(prov)),
		intruder:  agent.New("intruder", "Undeclared agent", agent.WithModel(prov)),
		root:      agent.New("root", "Root agent", agent.WithModel(prov)),
	}
	agent.WithSubAgents(f.librarian)(f.root)
	return f
}

// coldAuthRuntimeRestart admits one child, restarts over the same store, and returns the cold-path ids.
func coldAuthRuntimeRestart(t *testing.T, admitIntruder bool, mutate func(*LocalRuntime, string)) (*SessionService, session.Store, string, string) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	service := NewSessionService()
	f := newColdAuthFixture()
	first, err := service.NewRuntime(t.Context(), team.New(team.WithAgents(f.root, f.librarian, f.intruder)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	parentID := t.Name()
	h, err := first.CreateSession(t.Context(), session.New(session.WithID(parentID), session.WithUserMessage("Test"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	parent := h.(*sessionHandle).driver.session()

	var childID string
	if admitIntruder {
		child := newSubSession(parent, SubSessionConfig{AgentName: "intruder"}, f.intruder)
		require.NoError(t, first.admitDelegatedSession(parent, "root", child, f.intruder))
		childID = child.ID
	} else {
		result, err := first.handleTaskTransfer(t.Context(), parent, transferToolCall("librarian"), NewChannelSink(make(chan Event, 128)), tools.NopRuntime{})
		require.NoError(t, err)
		require.False(t, result.IsError)
		first.subagents.mu.Lock()
		for _, rec := range first.subagents.children {
			if rec.parentSession == parentID {
				childID = rec.sessionID
			}
		}
		first.subagents.mu.Unlock()
	}
	require.NotEmpty(t, childID)
	if mutate != nil {
		mutate(first, childID)
	}
	require.NoError(t, first.Close())

	g := newColdAuthFixture()
	second, err := service.NewRuntime(t.Context(), team.New(team.WithAgents(g.root, g.librarian, g.intruder)),
		WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	_, resident := second.sessionDrivers.Lookup(childID)
	require.False(t, resident, "attach must take the cold path")
	return service, store, parentID, childID
}

// Cold restore of a sync sub_agents child succeeds.
func TestColdRestoreSyncOnlySubagentRestores(t *testing.T) {
	t.Parallel()

	service, _, _, childID := coldAuthRuntimeRestart(t, false, nil)
	_, snapshot, err := service.LoadSession(t.Context(), childID)
	require.NoError(t, err)
	require.Equal(t, childID, snapshot.ID)
}

// Undeclared child: typed invalid restore_tree_membership.
func TestColdRestoreUndeclaredChildRejectedTyped(t *testing.T) {
	t.Parallel()

	service, _, _, childID := coldAuthRuntimeRestart(t, true, nil)
	_, _, err := service.LoadSession(t.Context(), childID)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorInvalid, sessionErr.Kind)
	require.Equal(t, SessionOperation("restore_tree_membership"), sessionErr.Operation)
}

// Cold parity: a recorded grantor that no longer resolves fails closed after restart.
func TestColdRestoreUnresolvedRecordedGrantorFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, grantor, grantorID string }{
		{name: "missing id with live name", grantor: "root", grantorID: "missing-grantor-id"},
		{name: "unresolved name without id", grantor: "ghost-grantor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, _, _, childID := coldAuthRuntimeRestart(t, false, func(rt *LocalRuntime, id string) {
				rewriteChildGrantor(t, rt, rt.sessionStore, id, tc.grantor, tc.grantorID)
			})
			_, _, err := service.LoadSession(t.Context(), childID)
			var sessionErr *SessionError
			require.ErrorAs(t, err, &sessionErr)
			require.Equal(t, SessionErrorInvalid, sessionErr.Kind)
			require.Equal(t, SessionOperation("restore_tree_membership"), sessionErr.Operation)
		})
	}
}
