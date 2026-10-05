package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

func attentionFixture(t *testing.T) (*subagentManager, *sessionDriver, SessionHandle, subagent.NodeID) {
	t.Helper()
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"), session.WithAgentName("root"))
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID), session.WithAgentName("worker"))
	const id = subagent.NodeID("child-node")
	m.registerChild(parent, "root", id, "worker", child)
	d, ok := m.r.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	h := &sessionHandle{runtime: m.r, driver: d, sessionID: child.ID, agentName: "worker"}
	return m, d, h, id
}

func resolveConfirmation(t *testing.T, d *sessionDriver, h SessionHandle, id string) {
	t.Helper()
	d.mu.Lock()
	delete(d.interactions, id)
	d.mu.Unlock()
	resume, err := d.registerResume(t.Context(), id)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- h.Respond(t.Context(), InteractionResponse{InteractionID: id, Kind: InteractionConfirmation, Resume: ResumeApprove()})
	}()
	<-resume
	require.NoError(t, <-done)
}

func resolveElicitation(t *testing.T, d *sessionDriver, h SessionHandle, interactionID, elicitationID string) {
	t.Helper()
	waiter := newElicitationWaiter()
	event := ElicitationRequest("question", "form", nil, "", elicitationID, "", d.identityID, nil, "worker").(*ElicitationRequestEvent)
	require.NoError(t, d.registerElicitation(t.Context(), interactionID, event, waiter))
	require.NoError(t, h.Respond(t.Context(), InteractionResponse{InteractionID: interactionID, Kind: InteractionElicitation, ElicitationID: elicitationID, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}}))
	<-waiter.ch
}

func TestAttentionMultipleInteractionsResolveConfirmationThenElicitation(t *testing.T) {
	m, d, h, id := attentionFixture(t)
	d.RegisterInteraction("confirm", InteractionConfirmation)
	d.RegisterInteraction("question", InteractionElicitation)
	resolveConfirmation(t, d, h, "confirm")
	node, _ := m.tree.Node(id)
	assert.Equal(t, "answer question", node.WaitingOn)
	resolveElicitation(t, d, h, "question", "elicitation-1")
	node, _ = m.tree.Node(id)
	assert.False(t, node.NeedsAttention)
}

func TestAttentionMultipleInteractionsResolveElicitationThenConfirmation(t *testing.T) {
	m, d, h, id := attentionFixture(t)
	d.RegisterInteraction("question", InteractionElicitation)
	d.RegisterInteraction("confirm", InteractionConfirmation)
	resolveElicitation(t, d, h, "question", "elicitation-2")
	node, _ := m.tree.Node(id)
	assert.Equal(t, "approve tool", node.WaitingOn)
	resolveConfirmation(t, d, h, "confirm")
	node, _ = m.tree.Node(id)
	assert.False(t, node.NeedsAttention)
}

func TestAttentionRootSessionReflectsOutstandingInteraction(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root-session"), session.WithAgentName("root"))
	d := m.r.sessionDrivers.Get(root)
	m.ensureRoot(root, "root")

	d.RegisterInteraction("confirm", InteractionConfirmation)
	node, ok := m.tree.Node(subagent.SessionRootID(root.ID))
	require.True(t, ok)
	assert.True(t, node.NeedsAttention)
	assert.Equal(t, "approve tool", node.WaitingOn)
}

func TestAttentionFailureOverridesOutstandingInteraction(t *testing.T) {
	m, d, _, id := attentionFixture(t)
	d.RegisterInteraction("question", InteractionElicitation)
	require.NoError(t, m.tree.Update(id, func(n *subagent.Node) { n.State = subagent.NodeFailed; n.NeedsAttention = true; n.WaitingOn = "failed" }))
	d.refreshAttention()
	node, _ := m.tree.Node(id)
	assert.True(t, node.NeedsAttention)
	assert.Equal(t, "failed", node.WaitingOn)
}

func TestAttentionAbandonedElicitationClearsExactWaiter(t *testing.T) {
	m, d, _, id := attentionFixture(t)
	waiter := newElicitationWaiter()
	event := ElicitationRequest("question", "form", nil, "", "question", "", d.identityID, nil, "worker").(*ElicitationRequestEvent)
	require.NoError(t, d.registerElicitation(t.Context(), "question", event, waiter))
	d.refreshAttention()
	node, _ := m.tree.Node(id)
	require.True(t, node.NeedsAttention)
	d.abandonElicitation("question", newElicitationWaiter())
	node, _ = m.tree.Node(id)
	assert.True(t, node.NeedsAttention)
	d.abandonElicitation("question", waiter)
	node, _ = m.tree.Node(id)
	assert.False(t, node.NeedsAttention)
}
