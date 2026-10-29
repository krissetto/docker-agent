package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// admissionProbeStore observes the real child admission: AdmitChild is the
// durable write that precedes driver activation in admitChildContextForTeam.
type admissionProbeStore struct {
	*session.InMemorySessionStore
	afterAdmit func(context.Context, session.ChildAdmission) error
}

func (s *admissionProbeStore) AdmitChild(ctx context.Context, a session.ChildAdmission) error {
	if err := s.InMemorySessionStore.AdmitChild(ctx, a); err != nil {
		return err
	}
	return s.afterAdmit(ctx, a)
}

// attachProbe is a real root with a real transfer_task child. declareAsync
// controls only whether the parent also declares the child in its async
// `subagents` list, which restore membership consults (allowedFromAgentForTeam
// reads AsyncSubagents, while transfer_task authorizes via SubAgents).
type attachProbe struct {
	rt       *LocalRuntime
	root     *agent.Agent
	intruder *agent.Agent
	service  *SessionService
	store    *admissionProbeStore
	parentID string
	parent   *session.Session
	childID  string
}

func newAttachProbe(t *testing.T, declareAsync bool) *attachProbe {
	t.Helper()
	p := &attachProbe{store: &admissionProbeStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}}
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("transferred").AddStopWithUsage(10, 5).Build()}
	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	intruder := agent.New("intruder", "Undeclared agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	agent.WithSubAgents(librarian)(root)
	if declareAsync {
		agent.WithAsyncSubagents(latest.SubagentRef{Agent: "librarian"})(root)
	}
	p.root, p.intruder = root, intruder

	p.service = NewSessionService()
	rt, err := p.service.NewRuntime(t.Context(), team.New(team.WithAgents(root, librarian, intruder)),
		WithSessionStore(p.store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	p.rt = rt

	p.parentID = t.Name()
	h, err := rt.CreateSession(t.Context(), session.New(session.WithID(p.parentID), session.WithUserMessage("Test"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	p.parent = h.(*sessionHandle).driver.session()

	p.store.afterAdmit = func(ctx context.Context, a session.ChildAdmission) error {
		p.childID = a.Record.Node.SessionID
		_, resident := rt.sessionDrivers.Lookup(p.childID)
		require.False(t, resident, "child driver must not be resident before activation")
		_, err := p.store.GetSession(ctx, p.childID)
		require.NoError(t, err, "child row must be durable before activation")
		return nil
	}
	return p
}

func (p *attachProbe) transfer(t *testing.T) {
	t.Helper()
	result, err := p.rt.handleTaskTransfer(t.Context(), p.parent, transferToolCall("librarian"), NewChannelSink(make(chan Event, 128)), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError)
	require.NotEmpty(t, p.childID, "probe must observe the real child admission")
}

// requireChildMembershipSnapshot mirrors, one check at a time, the checks in
// readResidentSessionViewLocked that return restore_tree_membership, so the
// first failing assertion names the exact invariant that diverged.
func requireChildMembershipSnapshot(t *testing.T, p *attachProbe) {
	t.Helper()
	rt, store, parentID, childID := p.rt, p.store, p.parentID, p.childID
	ctx := t.Context()
	record, err := store.LoadChild(ctx, childID)
	require.NoError(t, err, "stored child record")
	childRow, err := store.GetSessionMetadata(ctx, childID)
	require.NoError(t, err, "stored child row")
	parentRow, err := store.GetSessionMetadata(ctx, parentID)
	require.NoError(t, err, "stored parent row")

	rt.subagents.mu.Lock()
	rec := rt.subagents.children[record.Node.ID]
	tracked := rt.subagents.sessions[parentID]
	rootNode, rootOK := rt.subagents.tree.Node(subagent.SessionRootID(parentID))
	childNode, childOK := rt.subagents.tree.Node(record.Node.ID)
	rt.subagents.mu.Unlock()

	require.Equal(t, parentID, record.RootSessionID, "check: record.RootSessionID == root.ID")
	require.Equal(t, parentID, record.ParentSessionID, "check: record.ParentSessionID == parent.ID")
	require.Equal(t, childID, record.Node.SessionID, "check: record.Node.SessionID == row.ID")
	require.Equal(t, subagent.SessionRootID(parentID), record.Node.Parent, "check: record.Node.Parent == SessionRootID(root)")
	require.Equal(t, childRow.AttributesSnapshot()[SessionAgentAttribute], record.Node.Agent, "check: record.Node.Agent == row agent attr")

	childIdentity, _, childIdentityOK := identityForBinding(rt.teamForSession(childRow), childRow, SessionAgentAttribute, SessionAgentIDAttribute)
	require.True(t, childIdentityOK, "check: child identity resolves")
	grant, _, grantOK := identityForBinding(rt.teamForSession(childRow), childRow, SessionParentAgentAttribute, SessionParentAgentIDAttribute)
	if !grantOK {
		grant, _, grantOK = resolveAgentIdentity(rt.teamForSession(parentRow), parentRow.AgentID, parentRow.AgentName)
	}
	require.True(t, grantOK, "check: grant identity resolves")
	allowed := false
	for _, ref := range rt.allowedFromAgentForTeam(rt.teamForSession(parentRow), grant) {
		if ref.Agent == childIdentity.Name() {
			allowed = true
		}
	}
	require.True(t, allowed, "check: parent async subagents include child agent %q", childIdentity.Name())

	require.NotNil(t, tracked, "topology: root tracked in memory")
	require.True(t, tracked.topLevel, "topology: root tracked topLevel")
	require.True(t, rootOK, "topology: root tree node present")
	require.Equal(t, parentRow.AttributesSnapshot()[SessionAgentAttribute], rootNode.Agent, "topology: root tree agent == root row agent")
	require.NotNil(t, rec, "topology: child in memory")
	require.True(t, sameRestoredMembership(rec.durable, record), "topology: in-memory durable != stored record")
	require.True(t, childOK, "topology: child tree node present")
	require.Equal(t, record.Node.SessionID, childNode.SessionID, "topology: tree SessionID")
	require.Equal(t, record.Node.Parent, childNode.Parent, "topology: tree Parent")
	require.Equal(t, record.Node.Agent, childNode.Agent, "topology: tree Agent")
}

// Synchronous transfer_task must never expose a resident child without its
// stored row, and a declared parent-child pair must attach after the transfer.
func TestResidentAttachChildRowIsDurableBeforeDriverActivation(t *testing.T) {
	t.Parallel()

	p := newAttachProbe(t, true)
	p.transfer(t)

	driver, resident := p.rt.sessionDrivers.Lookup(p.childID)
	require.True(t, resident, "completed transfer leaves the child resident")

	requireChildMembershipSnapshot(t, p)

	handle, snapshot, err := p.service.LoadSession(t.Context(), p.childID)
	require.NoError(t, err)
	require.Equal(t, p.childID, snapshot.ID)
	require.Equal(t, p.parentID, snapshot.ParentID)
	require.Same(t, driver, handle.(*sessionHandle).driver)

	_, _, err = p.service.LoadSession(t.Context(), p.parentID)
	require.NoError(t, err)
}

// A child admitted by transfer_task under the sync sub_agents grant restores
// without any async declaration: the grant is the same authority admission used.
func TestResidentAttachSyncOnlySubagentRestores(t *testing.T) {
	t.Parallel()

	p := newAttachProbe(t, false)
	p.transfer(t)
	driver, resident := p.rt.sessionDrivers.Lookup(p.childID)
	require.True(t, resident)

	handle, snapshot, err := p.service.LoadSession(t.Context(), p.childID)
	require.NoError(t, err)
	require.Equal(t, p.childID, snapshot.ID)
	require.Equal(t, p.parentID, snapshot.ParentID)
	require.Same(t, driver, handle.(*sessionHandle).driver)
}

// Negative: a child whose agent neither grant names is rejected with the typed
// membership conflict, even though its row and tree node exist.
func TestResidentAttachUndeclaredChildRejectedTyped(t *testing.T) {
	t.Parallel()

	p := newAttachProbe(t, true)
	child := newSubSession(p.parent, SubSessionConfig{AgentName: "intruder"}, p.intruder)
	require.NoError(t, p.rt.admitDelegatedSession(p.parent, "root", child, p.intruder))

	_, _, err := p.service.LoadSession(t.Context(), child.ID)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorConflict, sessionErr.Kind)
	require.Equal(t, SessionOperation("restore_tree_membership"), sessionErr.Operation)
}

// Negative: removing the parent's sub_agents declaration revokes restore for a
// legacy child, because restore consults the grant at attach time.
func TestResidentAttachRemovedDeclarationRejectsLegacyChild(t *testing.T) {
	t.Parallel()

	p := newAttachProbe(t, false)
	p.transfer(t)
	_, _, err := p.service.LoadSession(t.Context(), p.childID)
	require.NoError(t, err)

	agent.WithSubAgents()(p.root)
	_, _, err = p.service.LoadSession(t.Context(), p.childID)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorConflict, sessionErr.Kind)
	require.Equal(t, SessionOperation("restore_tree_membership"), sessionErr.Operation)
}

// Unit contract for the two admission grants: sync sub_agents and async
// subagents each authorize by agent name; nothing else does.
func TestRestoreChildAuthorizedGrants(t *testing.T) {
	t.Parallel()

	p := newAttachProbe(t, false)
	tm := p.rt.teamForSession(p.parent)
	require.True(t, p.rt.restoreChildAuthorized(tm, p.root, "librarian"), "sync sub_agents grant")
	require.False(t, p.rt.restoreChildAuthorized(tm, p.root, "intruder"), "undeclared agent")
	require.False(t, p.rt.restoreChildAuthorized(tm, nil, "librarian"), "missing grantor")

	agent.WithAsyncSubagents(latest.SubagentRef{Agent: "intruder"})(p.root)
	require.True(t, p.rt.restoreChildAuthorized(tm, p.root, "intruder"), "async subagents grant")
}
