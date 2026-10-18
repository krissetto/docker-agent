package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type residentViewStore struct {
	*session.InMemorySessionStore
	reads, metadata, memberships, trees int
	recordChange                        func(*session.ChildRecord)
}

func (s *residentViewStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.reads++
	return s.InMemorySessionStore.GetSession(ctx, id)
}
func (s *residentViewStore) GetSessionMetadata(ctx context.Context, id string) (*session.Session, error) {
	s.metadata++
	row, err := s.InMemorySessionStore.GetSessionMetadata(ctx, id)
	return row, err
}
func (s *residentViewStore) LoadChild(ctx context.Context, id string) (session.ChildRecord, error) {
	s.memberships++
	record, err := s.InMemorySessionStore.LoadChild(ctx, id)
	if err == nil && s.recordChange != nil {
		s.recordChange(&record)
	}
	return record, err
}
func (s *residentViewStore) LoadChildren(ctx context.Context, id string) ([]session.ChildRecord, error) {
	s.trees++
	return s.InMemorySessionStore.LoadChildren(ctx, id)
}

func residentViewFixture(t *testing.T, count, turns int) (*LocalRuntime, SessionRuntime, *residentViewStore) {
	t.Helper()
	store := &residentViewStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
	root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	for i := range count {
		child := session.New(session.WithID(fmt.Sprintf("child-%d", i)), session.WithParentID("root"), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
		history := turns
		if i == 0 {
			history = 10
		}
		for range history {
			child.AddMessage(session.UserMessage("retained own history"))
		}
		require.NoError(t, store.AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: session.ChildRecord{RootSessionID: "root", ParentSessionID: "root", Node: subagent.Node{ID: subagent.NodeID(child.ID), SessionID: child.ID, Parent: subagent.SessionRootID("root"), Agent: "worker", State: subagent.NodeCompleted}}}))
	}
	r, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	_, err := OpenSessionView(t.Context(), owner.Runtime(), "child-0")
	require.NoError(t, err)
	store.reads, store.metadata, store.memberships, store.trees = 0, 0, 0, 0
	return r, owner.Runtime(), store
}

func TestResidentSessionViewReadsOnlySelectedAncestry(t *testing.T) {
	for _, n := range []int{1, 10, 128} {
		for _, turns := range []int{0, 100} {
			t.Run(fmt.Sprintf("children%d/turns%d", n, turns), func(t *testing.T) {
				r, view, store := residentViewFixture(t, n, turns)
				original, err := view.SessionByID("child-0")
				require.NoError(t, err)
				prepared, err := view.(SessionViewPreparer).PrepareSessionView(t.Context(), "child-0")
				require.NoError(t, err)
				defer prepared.Abort()
				assert.True(t, prepared.(*preparedSessionView).resident)
				assert.Equal(t, 2, store.metadata)
				assert.Equal(t, 1, store.memberships)
				info := prepared.Info()
				assert.Len(t, info.Session.Messages, 10)
				assert.Nil(t, info.Session.GetSubagentTree())
				assert.Equal(t, "root", info.RootBinding.AgentName)
				committed, err := prepared.Commit(t.Context())
				require.NoError(t, err)
				assert.Same(t, original.(*sessionHandle).driver, committed.SessionHandle.(*sessionHandle).driver)
				assert.Len(t, committed.Info.Session.Messages, 10)
				assert.Nil(t, committed.Info.Session.GetSubagentTree())
				assert.Zero(t, store.reads)
				assert.Zero(t, store.trees)
				assert.Equal(t, 4, store.metadata)
				assert.Equal(t, 2, store.memberships)
				assert.Len(t, r.sessionDrivers.drivers, n+1)
			})
		}
	}
}

func TestResidentSessionViewRevalidatesPersistedIdentityAtCommit(t *testing.T) {
	for _, change := range []string{"parent", "binding", "source", "workspace", "root workspace", "root binding", "wrong root", "node", "membership parent", "canonical index", "deleted", "stopped"} {
		t.Run(change, func(t *testing.T) {
			r, view, store := residentViewFixture(t, 2, 1)
			prepared, err := view.(SessionViewPreparer).PrepareSessionView(t.Context(), "child-0")
			require.NoError(t, err)
			defer prepared.Abort()
			if change == "canonical index" {
				r.subagents.mu.Lock()
				r.subagents.children["child-0"].durable.ParentSessionID = "foreign"
				r.subagents.mu.Unlock()
			} else if change == "deleted" {
				require.NoError(t, store.DeleteSession(t.Context(), "child-0"))
			} else if change == "stopped" {
				record, err := store.LoadChild(t.Context(), "child-0")
				require.NoError(t, err)
				record.Node.State = subagent.NodeStopped
				require.NoError(t, store.CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: record.Revision, Record: record}))
			} else if change == "wrong root" || change == "node" || change == "membership parent" {
				store.recordChange = func(record *session.ChildRecord) {
					switch change {
					case "wrong root":
						record.RootSessionID = "foreign"
					case "node":
						record.Node.ID = "foreign"
					case "membership parent":
						record.Node.Parent = "foreign"
					}
				}
			} else {
				id := "child-0"
				if change == "root workspace" || change == "root binding" {
					id = "root"
				}
				row, err := store.GetSession(t.Context(), id)
				require.NoError(t, err)
				switch change {
				case "parent":
					row.ParentID = "foreign"
				case "binding", "root binding":
					row.SetAttribute(SessionAgentAttribute, "foreign")
				case "source":
					row.SetAttribute("docker-agent.actor.source", "foreign")
				case "workspace", "root workspace":
					row.WorkingDir = "/foreign"
				}
				require.NoError(t, store.UpdateSession(t.Context(), row))
			}
			_, err = prepared.Commit(t.Context())
			require.Error(t, err)
			again, err := view.(SessionViewPreparer).PrepareSessionView(t.Context(), "child-0")
			if change == "stopped" {
				// A newly stopped owner may be replaced only by the cold stopped-view path.
				require.NoError(t, err)
				again.Abort()
			} else {
				require.Error(t, err)
			}
		})
	}
}

type legacyResidentStore struct {
	session.Store
	session.CoordinationStore
	session.ChildAdmissionBatchStore
	reads int
}

func (s *legacyResidentStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.reads++
	return s.Store.GetSession(ctx, id)
}

func TestResidentSessionViewStoreWithoutTargetedReadsKeepsRestore(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &legacyResidentStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ChildAdmissionBatchStore: base.(session.ChildAdmissionBatchStore)}
	_, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	_, err := OpenSessionView(t.Context(), owner.Runtime(), root.ID)
	require.NoError(t, err)
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	defer prepared.Abort()
	assert.False(t, prepared.(*preparedSessionView).resident)
	assert.GreaterOrEqual(t, store.reads, 2)
}

func TestSessionByIDUsesMetadataButSnapshotPreservesHistory(t *testing.T) {
	_, view, _ := residentViewFixture(t, 3, 10)
	allocations := testing.AllocsPerRun(5, func() {
		_, err := view.SessionByID("root")
		if err != nil {
			panic(err)
		}
	})
	assert.Less(t, allocations, float64(20), "handle lookup must not clone descendant transcripts")
	handle, err := view.SessionByID("root")
	require.NoError(t, err)
	assert.Equal(t, "root", handle.AgentName())
	assert.Equal(t, "root", handle.Metadata().AgentName)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 3)
	require.NotNil(t, snapshot.Messages[0].SubSession)
	assert.Len(t, snapshot.Messages[0].SubSession.Messages, 10)
	snapshot.Messages[0].SubSession.Messages[0].Message.Message.Content = "mutated"
	again, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "retained own history", again.Messages[0].SubSession.Messages[0].Message.Message.Content)
}

func TestResidentSessionViewCommitsFreshUntrackedRoot(t *testing.T) {
	r, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("reply"), coordinationReply("unused"))
	handle := coordinationCreate(t, owner.Runtime(), "fresh-root", "")
	rootID := subagent.SessionRootID(handle.ID())
	_, treeExists := r.subagents.tree.Node(rootID)
	assert.False(t, treeExists)
	r.subagents.mu.Lock()
	_, tracked := r.subagents.sessions[handle.ID()]
	r.subagents.mu.Unlock()
	assert.False(t, tracked)

	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), handle.ID())
	require.NoError(t, err)
	defer prepared.Abort()
	assert.True(t, prepared.(*preparedSessionView).resident)
	_, treeExists = r.subagents.tree.Node(rootID)
	assert.False(t, treeExists, "read-only preparation must not publish topology")

	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, handle.ID(), committed.Info.RootSessionID)
	assert.Equal(t, "root", committed.Info.RootBinding.AgentName)
	node, treeExists := r.subagents.tree.Node(rootID)
	require.True(t, treeExists)
	assert.Equal(t, "root", node.Agent)
	r.subagents.mu.Lock()
	trackedRoot := r.subagents.sessions[handle.ID()]
	r.subagents.mu.Unlock()
	require.NotNil(t, trackedRoot)
	assert.True(t, trackedRoot.topLevel)
	assert.Nil(t, trackedRoot.sess, "published resident driver owns the root snapshot")

	submission, err := committed.SessionHandle.Submit(t.Context(), TurnInput{Content: "first managed prompt", RequestID: "first-managed-prompt"})
	require.NoError(t, err)
	require.NoError(t, committed.SessionHandle.AwaitTurn(t.Context(), submission.TurnID))
	snapshot, err := committed.SessionHandle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "reply", snapshot.GetLastAssistantMessageContent())
}

func TestResidentSessionViewRejectsPartialUntrackedRootTopology(t *testing.T) {
	for _, partial := range []string{"tree", "tracking", "child record"} {
		t.Run(partial, func(t *testing.T) {
			r, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("unused"), coordinationReply("unused"))
			handle := coordinationCreate(t, owner.Runtime(), "partial-root", "")
			rootID := subagent.SessionRootID(handle.ID())
			r.subagents.mu.Lock()
			switch partial {
			case "tree":
				require.NoError(t, r.subagents.tree.Add(subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}))
			case "tracking":
				r.subagents.sessions[handle.ID()] = &sessionSubagents{node: rootID, topLevel: true}
			case "child record":
				childID := subagent.NodeID("orphan-child")
				r.subagents.children[childID] = &childRecord{sessionID: "orphan-child", parentSession: handle.ID(), durable: session.ChildRecord{RootSessionID: handle.ID(), ParentSessionID: handle.ID(), Node: subagent.Node{ID: childID, SessionID: "orphan-child", Parent: rootID, Agent: "worker"}}}
			}
			r.subagents.mu.Unlock()

			_, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), handle.ID())
			require.Error(t, err)
			var sessionErr *SessionError
			require.ErrorAs(t, err, &sessionErr)
			assert.Equal(t, SessionErrorConflict, sessionErr.Kind)
			assert.Equal(t, SessionOperationRestoreTreeMembership, sessionErr.Operation)
		})
	}
}

func TestResidentFreshRootDoesNotSeedTopologyOnFailedCommit(t *testing.T) {
	for _, fail := range []string{"canceled", "binding changed"} {
		t.Run(fail, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			r, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
			handle := coordinationCreate(t, owner.Runtime(), "uncommitted-root", "")
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), handle.ID())
			require.NoError(t, err)
			defer prepared.Abort()
			ctx := t.Context()
			if fail == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			} else {
				row, err := store.GetSession(t.Context(), handle.ID())
				require.NoError(t, err)
				row.SetAttribute(SessionAgentAttribute, "foreign")
				require.NoError(t, store.UpdateSession(t.Context(), row))
			}
			_, err = prepared.Commit(ctx)
			require.Error(t, err)
			_, treeExists := r.subagents.tree.Node(subagent.SessionRootID(handle.ID()))
			assert.False(t, treeExists, "failed confirmation must not seed topology")
			r.subagents.mu.Lock()
			_, tracked := r.subagents.sessions[handle.ID()]
			r.subagents.mu.Unlock()
			assert.False(t, tracked, "failed confirmation must not publish tracking")
		})
	}
}

func TestResidentFreshRootWithStoredChildrenUsesColdRestore(t *testing.T) {
	store := session.NewInMemorySessionStore()
	_, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	root := coordinationCreate(t, owner.Runtime(), "root-with-child-record", "")
	child := session.New(session.WithID("persisted-child"), session.WithParentID(root.ID()), session.WithAgentName("worker"), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: "root"}))
	record := session.ChildRecord{RootSessionID: root.ID(), ParentSessionID: root.ID(), Node: subagent.Node{ID: subagent.NodeID(child.ID), SessionID: child.ID, Parent: subagent.SessionRootID(root.ID()), Agent: "worker", State: subagent.NodeCompleted}}
	require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))

	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID())
	require.NoError(t, err)
	defer prepared.Abort()
	assert.False(t, prepared.(*preparedSessionView).resident, "an untracked root with durable descendants uses the full cold preflight")
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, root.ID(), committed.Info.RootSessionID)
	rt := owner.Runtime().(*localSessionRuntimeView).runtime
	_, ok := rt.sessionDrivers.Lookup(child.ID)
	assert.True(t, ok, "valid persisted descendants are restored rather than hidden or rejected")
}
