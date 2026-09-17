package runtime

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type ancestryViewStore struct {
	*session.InMemorySessionStore

	reads        []string
	ids          []string
	afterRead    func(int)
	admissions   int
	writes       int
	childLoads   int
	childRecords []session.ChildRecord
}

func (s *ancestryViewStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.reads = append(s.reads, id)
	loaded, err := s.InMemorySessionStore.GetSession(ctx, id)
	if s.afterRead != nil {
		s.afterRead(len(s.reads))
	}
	return loaded, err
}

func (s *ancestryViewStore) LoadChildren(ctx context.Context, rootID string) ([]session.ChildRecord, error) {
	s.childLoads++
	if s.childRecords != nil {
		return s.childRecords, nil
	}
	return s.InMemorySessionStore.LoadChildren(ctx, rootID)
}

func (s *ancestryViewStore) AddSession(ctx context.Context, sess *session.Session) error {
	s.writes++
	return s.InMemorySessionStore.AddSession(ctx, sess)
}

func (s *ancestryViewStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	s.writes++
	return s.InMemorySessionStore.UpdateSession(ctx, sess)
}

func (s *ancestryViewStore) PromotePendingUserMessage(ctx context.Context, id, turnID string) error {
	s.writes++
	return s.InMemorySessionStore.PromotePendingUserMessage(ctx, id, turnID)
}

func (s *ancestryViewStore) AdmitChild(ctx context.Context, admission session.ChildAdmission) error {
	s.admissions++
	return s.InMemorySessionStore.AdmitChild(ctx, admission)
}

func (s *ancestryViewStore) AdmitChildren(ctx context.Context, admissions []session.ChildAdmission) error {
	s.admissions++
	return s.InMemorySessionStore.AdmitChildren(ctx, admissions)
}

func ancestryViewChain(t *testing.T, count int) (*ancestryViewStore, []*session.Session) {
	t.Helper()
	store := &ancestryViewStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
	chain := make([]*session.Session, count)
	for i := range chain {
		binding, parent := "root", ""
		if i > 0 {
			binding, parent = "worker", chain[i-1].ID
		}
		chain[i] = session.New(session.WithID(fmt.Sprintf("ancestry-%d", i)), session.WithParentID(parent), session.WithAttributes(map[string]string{
			SessionAgentAttribute: binding, SessionParentAgentAttribute: "root", "docker-agent.actor.source": "ancestry-test",
		}))
		pending := session.UserMessage("archived FIFO")
		pending.TurnID, pending.Pending, pending.Accepted = fmt.Sprintf("pending-%d", i), true, true
		pending.InputOrigin = session.InputOriginUser
		chain[i].AddMessage(pending)
		require.NoError(t, store.InMemorySessionStore.AddSession(t.Context(), chain[i]))
		store.ids = append(store.ids, chain[i].ID)
	}
	return store, chain
}

func ancestryViewRows(t *testing.T, store *ancestryViewStore) map[string]*session.Session {
	t.Helper()
	out := make(map[string]*session.Session, len(store.ids))
	for _, id := range store.ids {
		row, err := store.InMemorySessionStore.GetSession(t.Context(), id)
		require.NoError(t, err)
		out[row.ID] = row.OwnSnapshot()
	}
	return out
}

func assertAncestryViewUnpublished(t *testing.T, rt *LocalRuntime, store *ancestryViewStore, before map[string]*session.Session) {
	t.Helper()
	assert.Zero(t, store.admissions)
	assert.Zero(t, store.writes)
	assert.Equal(t, before, ancestryViewRows(t, store), "preparation must not rewrite ancestry, bindings or accepted FIFO")
	assert.Empty(t, rt.subagents.tree.Snapshot().Nodes)
	func() {
		rt.subagents.mu.Lock()
		defer rt.subagents.mu.Unlock()
		assert.Empty(t, rt.subagents.sessions)
		assert.Empty(t, rt.subagents.children)
	}()
	func() {
		rt.sessionDrivers.mu.Lock()
		defer rt.sessionDrivers.mu.Unlock()
		assert.Empty(t, rt.sessionDrivers.drivers)
		assert.Empty(t, rt.sessionDrivers.reservations)
	}()
}

func TestSessionViewAncestryRejectsBoundCycleAndCancellationWithoutSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      int
		count      int
		cycle      bool
		cancelRead int
		wantReads  int
		wantLimit  int
	}{
		{name: "long acyclic", limit: 6, count: 18, wantReads: 6, wantLimit: 6},
		{name: "unlimited residency remains bounded", limit: UnlimitedSessionResources, count: DefaultSessionResourcePolicy().MaxSessions + 1, wantReads: DefaultSessionResourcePolicy().MaxSessions, wantLimit: DefaultSessionResourcePolicy().MaxSessions},
		{name: "zero capacity", limit: 0, count: 2, wantReads: 1},
		{name: "cycle", limit: 6, count: 3, cycle: true, wantReads: 4},
		{name: "already canceled", limit: 6, count: 3, cancelRead: -1},
		{name: "canceled after selected read", limit: 6, count: 3, cancelRead: 1, wantReads: 1},
		{name: "canceled during ancestry", limit: 6, count: 4, cancelRead: 2, wantReads: 2},
		{name: "canceled after root read", limit: 6, count: 3, cancelRead: 3, wantReads: 3},
		{name: "cancellation at capacity", limit: 2, count: 4, cancelRead: 2, wantReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, chain := ancestryViewChain(t, tc.count)
			if tc.cycle {
				chain[0].ParentID = chain[len(chain)-1].ID
				require.NoError(t, store.InMemorySessionStore.UpdateSession(t.Context(), chain[0]))
			}
			var providerCalls atomic.Int32
			provider := coordinationReply("unused")
			reply := provider.call
			provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
				providerCalls.Add(1)
				return reply(ctx, messages)
			}
			rt, owner := coordinationRuntime(t, store, provider, provider)
			rt.maxSessions = tc.limit
			before := ancestryViewRows(t, store)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelRead < 0 {
				cancel()
			} else if tc.cancelRead > 0 {
				store.afterRead = func(n int) {
					if n == tc.cancelRead {
						cancel()
					}
				}
			}
			selected := chain[len(chain)-1]
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(ctx, selected.ID)
			require.Error(t, err)
			assert.Nil(t, prepared)
			if tc.cancelRead != 0 {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				var typed *SessionError
				require.ErrorAs(t, err, &typed)
				assert.Equal(t, selected.ID, typed.SessionID)
				assert.Equal(t, SessionOperation("restore_ancestry"), typed.Operation)
				if tc.cycle {
					assert.Equal(t, SessionErrorInvalid, typed.Kind)
				} else {
					assert.Equal(t, SessionErrorCapacity, typed.Kind)
					assert.Equal(t, SessionErrorReasonLimit, typed.Reason)
					assert.Equal(t, tc.wantLimit, typed.Limit)
				}
			}
			assert.Len(t, store.reads, tc.wantReads, "stop before the next expensive ancestor read")
			assertAncestryViewUnpublished(t, rt, store, before)
			records, err := store.LoadChildren(t.Context(), chain[0].ID)
			require.NoError(t, err)
			assert.Empty(t, records)
			assert.Zero(t, providerCalls.Load(), "rejected preparation must never wake accepted work")
		})
	}
}

func TestSessionViewAncestryAtCapacityPreservesArchivedChildBeyondSpawnDepth(t *testing.T) {
	const count = defaultMaxSubagentDepth + 2
	store, chain := ancestryViewChain(t, count)
	rt, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
	rt.maxSessions = count
	rootID := subagent.SessionRootID(chain[0].ID)
	var branch subagent.NodeSnapshot
	for i, sess := range slices.Backward(chain) {
		node := subagent.Node{ID: subagent.NodeID(sess.ID), SessionID: sess.ID, Agent: "worker", State: subagent.NodeIdle}
		switch i {
		case 0:
			node.ID, node.SessionID, node.Agent = rootID, "", "root"
		case 1:
			node.Parent = rootID
		default:
			node.Parent = subagent.NodeID(chain[i-1].ID)
		}
		next := subagent.NodeSnapshot{Node: node}
		if i < len(chain)-1 {
			next.Children = []subagent.NodeSnapshot{branch}
		}
		branch = next
	}
	snapshot := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootID, Nodes: []subagent.NodeSnapshot{branch}}
	require.NoError(t, rt.subagentStore.SaveTree(t.Context(), chain[0].ID, snapshot))
	before := ancestryViewRows(t, store)
	selected := chain[len(chain)-1]
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), selected.ID)
	require.NoError(t, err)
	defer prepared.Abort()
	info := prepared.Info()
	assert.Equal(t, selected.ID, info.SessionID)
	assert.Equal(t, chain[0].ID, info.RootSessionID)
	assert.Equal(t, selected.ParentID, info.Binding.ParentSessionID)
	assert.Equal(t, "worker", info.Binding.AgentName)
	require.NotNil(t, info.Attach)
	assert.Equal(t, selected.ParentID, info.Attach.ParentSessionID)
	require.GreaterOrEqual(t, len(store.reads), count)
	for i := range count {
		assert.Equal(t, chain[count-1-i].ID, store.reads[i], "ancestry reads reach the canonical root at the inclusive capacity boundary")
	}
	assert.Len(t, store.reads, count+count-1, "only ancestry and tree preflight read sessions")
	prepared.Abort()
	assertAncestryViewUnpublished(t, rt, store, before)
	records, err := store.LoadChildren(t.Context(), chain[0].ID)
	require.NoError(t, err)
	assert.Empty(t, records, "valid preparation does not migrate legacy children before commit")
	stored, err := rt.subagentStore.LoadTree(t.Context(), chain[0].ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, snapshot, *stored)
}

func TestSessionViewTreeHydrationBudgetCountsStoppedHistoryBeforePreflight(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     int
		canonical  int
		deep       bool
		limit      int
		valid      bool
		childLoads int
	}{
		{name: "wide legacy", legacy: 10, limit: 4},
		{name: "deep legacy", legacy: 10, limit: 4, deep: true},
		{name: "canonical records", canonical: 10, limit: 4, childLoads: 1},
		{name: "merged records and legacy", legacy: 2, canonical: 2, limit: 4, childLoads: 1},
		{name: "zero capacity root", limit: 0},
		{name: "exact bound stopped legacy", legacy: 3, limit: 4, valid: true, childLoads: 1},
		{name: "exact bound stopped canonical", canonical: 3, limit: 4, valid: true, childLoads: 1},
		{name: "exact bound merged history", legacy: 2, canonical: 1, limit: 4, valid: true, childLoads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, chain := ancestryViewChain(t, 1)
			root := chain[0]
			rootID := subagent.SessionRootID(root.ID)
			snapshot := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}}}}
			children := &snapshot.Nodes[0].Children
			for i := range tc.legacy {
				parent := rootID
				if tc.deep && i > 0 {
					parent = subagent.NodeID(fmt.Sprintf("legacy-%d", i-1))
				}
				node := subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("legacy-%d", i)), Parent: parent, Agent: "worker", State: subagent.NodeStopped}}
				if tc.deep {
					// Sessionful stopped history may retain descendants; missing
					// rows are not recreated during a view preparation.
					node.Node.SessionID = fmt.Sprintf("missing-%d", i)
				}
				*children = append(*children, node)
				if tc.deep {
					children = &(*children)[0].Children
				}
			}
			for i := range tc.canonical {
				store.childRecords = append(store.childRecords, session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Revision: 1, Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("canonical-%d", i)), Parent: rootID, Agent: "worker", State: subagent.NodeStopped}})
			}
			var providerCalls atomic.Int32
			provider := coordinationReply("unused")
			reply := provider.call
			provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
				providerCalls.Add(1)
				return reply(ctx, messages)
			}
			rt, owner := coordinationRuntime(t, store, provider, provider)
			rt.maxSessions = tc.limit
			require.NoError(t, rt.subagentStore.SaveTree(t.Context(), root.ID, snapshot))
			before := ancestryViewRows(t, store)
			beforeRecords := append([]session.ChildRecord(nil), store.childRecords...)
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
			if tc.valid {
				require.NoError(t, err)
				prepared.Abort()
			} else {
				require.Error(t, err)
				assert.Nil(t, prepared)
				var typed *SessionError
				require.ErrorAs(t, err, &typed)
				assert.Equal(t, SessionErrorCapacity, typed.Kind)
				assert.Equal(t, SessionOperation("restore_tree"), typed.Operation)
				assert.Equal(t, SessionErrorReasonLimit, typed.Reason)
				assert.Equal(t, tc.limit, typed.Limit)
			}
			assert.Equal(t, []string{root.ID}, store.reads, "tree budget rejects before loading any historical child sessions")
			assert.Equal(t, tc.childLoads, store.childLoads)
			assertAncestryViewUnpublished(t, rt, store, before)
			assert.Equal(t, beforeRecords, store.childRecords)
			stored, err := rt.subagentStore.LoadTree(t.Context(), root.ID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, snapshot, *stored)
			assert.Zero(t, providerCalls.Load())
		})
	}
}
