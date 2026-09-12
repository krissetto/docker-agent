package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type admissionFailTreeStore struct {
	*subagent.InMemoryStore

	remaining int
	always    bool
}

func (s *admissionFailTreeStore) SaveTree(ctx context.Context, id string, snapshot subagent.Snapshot) error {
	if s.always || s.remaining > 0 {
		if s.remaining > 0 {
			s.remaining--
		}
		return errors.New("injected tree save failure")
	}
	return s.InMemoryStore.SaveTree(ctx, id, snapshot)
}

func testTreeAdmissionRollback(t *testing.T, store *admissionFailTreeStore) {
	t.Helper()
	m := newTestSubagentManager(t)
	m.r.subagentStore = store
	parent := session.New(session.WithID("parent"), session.WithAgentName("root"))
	m.ensureRoot(parent, "root")
	prior := m.tree.Snapshot()
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID), session.WithAgentName("worker"))
	err := m.registerIdleChild(parent, "root", child, nil, subagent.AllowedSubagent{Agent: "worker"})
	require.ErrorContains(t, err, "injected")
	assert.Equal(t, prior, m.tree.Snapshot(), "failed admission restores topology")
	_, tracked := m.sessions[child.ID]
	assert.False(t, tracked)
	_, trackedNode := m.nodeForSession(child.ID)
	assert.False(t, trackedNode)
	mirrored := parent.GetSubagentTree()
	assert.True(t, mirrored == nil || len(mirrored.Nodes) == 0, "uncommitted child is not mirrored")
}

func TestClientChildTreePersistenceRollbackFailOnce(t *testing.T) {
	testTreeAdmissionRollback(t, &admissionFailTreeStore{InMemoryStore: subagent.NewInMemoryStore(), remaining: 1})
}

func TestClientChildTreePersistenceRollbackFailAlways(t *testing.T) {
	testTreeAdmissionRollback(t, &admissionFailTreeStore{InMemoryStore: subagent.NewInMemoryStore(), always: true})
}

func TestTreeObservationUnexpectedDescendantClosureIsTerminal(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAgentName("worker"))
	m.ensureRoot(root, "root")
	require.NoError(t, m.tree.Add(subagent.Node{ID: "child-node", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, State: subagent.NodeIdle}))
	h := &sessionHandle{runtime: m.r, driver: m.r.sessionDrivers.Get(root), sessionID: root.ID, agentName: "root"}
	childEvents, childErrors := make(chan SessionEvent), make(chan error)
	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	observe := func(_ context.Context, id string, _ ObserveOptions) (Observation, error) {
		events, errorsCh := make(chan SessionEvent), make(chan error)
		cancel := func() {}
		if id == child.ID {
			events, errorsCh, cancel = childEvents, childErrors, func() { cancelOnce.Do(func() { close(cancelled) }) }
		}
		return Observation{Initial: []SessionSnapshot{SessionSnapshot{Session: session.New(session.WithID(id)), Status: SessionStatus{SessionID: id}}}, Events: events, Errors: errorsCh, Cancel: cancel}, nil
	}
	observation, err := h.observeTreeWith(t.Context(), ObserveOptions{Tree: true}, observe)
	require.NoError(t, err)
	close(childErrors)
	close(childEvents)
	select {
	case terminal := <-observation.Errors:
		require.ErrorContains(t, terminal, "descendant observation closed: child")
	case <-time.After(time.Second):
		t.Fatal("missing terminal closure error")
	}
	select {
	case <-observation.Events:
	case <-time.After(time.Second):
		t.Fatal("multiplexed output did not close")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("descendant observation was not cancelled")
	}
}

func TestTreeObservationBufferOneManyInitializedDescendants(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"), session.WithAgentName("root"))
	rootDriver := m.r.sessionDrivers.Get(root)
	m.ensureRoot(root, "root")
	for i := range 32 {
		id := "child-" + string(rune('a'+i))
		child := session.New(session.WithID(id), session.WithParentID(root.ID), session.WithAgentName("worker"))
		m.r.sessionDrivers.Get(child).events.Publish(id, StreamStarted(id, "worker"))
		require.NoError(t, m.tree.Add(subagent.Node{ID: subagent.NodeID("node-" + id), Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: id, State: subagent.NodeRunning}))
	}
	h := &sessionHandle{runtime: m.r, driver: rootDriver, sessionID: root.ID, agentName: "root"}
	result := make(chan Observation, 1)
	errs := make(chan error, 1)
	go func() {
		observation, err := h.Observe(t.Context(), ObserveOptions{Tree: true, Buffer: 1})
		if err != nil {
			errs <- err
			return
		}
		result <- observation
	}()
	select {
	case err := <-errs:
		require.NoError(t, err)
	case observation := <-result:
		require.Len(t, observation.Initial, 33)
		require.Len(t, observation.Replay, 32)
		observation.Cancel()
		select {
		case <-observation.Events:
		case <-time.After(time.Second):
			t.Fatal("observation did not cleanly cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("tree observation blocked with Buffer=1")
	}
}
