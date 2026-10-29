package runtime

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCommunicationRetryIdentity(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("communication-parent"))
	child := session.New(session.WithID("communication-child"))
	m.registerChild(parent, "root", "abcde", "worker", child)
	store := session.NewInMemorySessionStore()
	m.r.sessionStore = store
	require.NoError(t, store.AddSession(t.Context(), child.OwnSnapshot()))
	d, ok := m.r.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.activeRequestID = "active"; return nil }))
	msg := agentCommunication("do this", "stable-request", parent.ID, "root", subagent.DeliveryGuidance)
	first, err := d.postCommunication(t.Context(), msg)
	require.NoError(t, err)
	assert.True(t, first.Accepted)
	assert.True(t, first.Durable)
	assert.False(t, first.Idempotent)
	assert.False(t, first.Queued, "active guidance is admitted directly to safe-boundary consumption")
	retry, err := d.postCommunication(t.Context(), msg)
	require.NoError(t, err)
	assert.True(t, retry.Idempotent)
	assert.True(t, retry.Durable)
	assert.Equal(t, first.Queued, retry.Queued, "exact retry must preserve its delivery receipt")
	require.Len(t, d.pending, 1)
	stored, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)
	require.Len(t, stored.MessagesSnapshot(), 1)
	for _, change := range []func(*QueuedMessage){
		func(m *QueuedMessage) { m.Content = "different" },
		func(m *QueuedMessage) { m.InputMode = "turn" },
		func(m *QueuedMessage) { m.SenderID = "other" },
		func(m *QueuedMessage) { m.SenderName = "other" },
	} {
		changed := msg
		change(&changed)
		rejected, err := d.postCommunication(t.Context(), changed)
		require.Error(t, err)
		assert.False(t, rejected.Accepted)
		assert.Equal(t, "conflict", communicationRejection(rejected, err).Rejection)
	}
	require.Len(t, d.pending, 1)
}

func TestCommunicationGuidanceAndNewTurn(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("boundary-parent"))
	child := session.New(session.WithID("boundary-child"))
	m.registerChild(parent, "root", "abcde", "worker", child)
	d, _ := m.r.sessionDrivers.Lookup(child.ID)
	ordinary := QueuedMessage{Content: "older user turn", RequestID: "user-turn", InputMode: "turn"}
	_, err := d.postCommunication(t.Context(), ordinary)
	require.NoError(t, err)
	followup, err := d.postCommunication(t.Context(), agentCommunication("later task", "followup", parent.ID, "root", subagent.DeliveryNewTurn))
	require.NoError(t, err)
	assert.Equal(t, subagent.DeliveryNewTurn, followup.Disposition)
	assert.True(t, followup.Queued)
	guidance, err := d.postCommunication(t.Context(), agentCommunication("correction", "guidance", parent.ID, "root", subagent.DeliveryGuidance))
	require.NoError(t, err)
	assert.Equal(t, subagent.DeliveryGuidance, guidance.Disposition)
	assert.False(t, guidance.Durable)
	promoted := d.drainBoundarySteering()
	require.Len(t, promoted, 1)
	assert.Equal(t, "guidance", promoted[0].RequestID)
	assert.Equal(t, session.InputOriginAgent, promoted[0].InputOrigin)
	assert.Equal(t, parent.ID, promoted[0].SenderID)
	assert.Equal(t, "root", promoted[0].SenderName)
	require.Len(t, d.pending, 2)
	assert.Equal(t, "user-turn", d.pending[0].RequestID)
	assert.Equal(t, "followup", d.pending[1].RequestID)
	assert.False(t, d.hasGuidanceLocked())
	// A retry after promotion must not insert guidance into active work again.
	retry, err := d.postCommunication(t.Context(), agentCommunication("correction", "guidance", parent.ID, "root", subagent.DeliveryGuidance))
	require.NoError(t, err)
	assert.True(t, retry.Idempotent)
	assert.False(t, retry.Queued)
	assert.Empty(t, d.drainBoundarySteering())
}

func TestCommunicationRejectsAbsentParentAndUnauthorizedChild(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("absent-parent"))
	child := session.New(session.WithID("owned-child"))
	m.registerChild(parent, "root", "abcde", "worker", child)
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("worker", "worker")))
	m.r.agents = newAgentRouter(m.r.team, "root")
	child.AgentName = "worker"
	receipt, err := m.sendCommunicationToChild(t.Context(), "unrelated", "abcde", "secret", "denied", subagent.DeliveryGuidance)
	require.Error(t, err)
	assert.False(t, receipt.Accepted)
	assert.Equal(t, "unauthorized", communicationRejection(receipt, err).Rejection)
	result, err := m.r.handleSendMessage(t.Context(), child, tools.ToolCall{ID: "tool-send", Function: tools.FunctionCall{Arguments: `{"to":"parent","message":"update","request_id":"parent-request"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
	assert.False(t, receipt.Accepted)
	assert.False(t, receipt.Durable)
	assert.Equal(t, "not_found", receipt.Rejection)
	assert.False(t, m.deliverExplicitToParent(parent.ID, "update", child.ID, "worker"))
}

func TestCommunicationToolCallRetryAndMissingStorageRow(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("tool-parent"))
	child := session.New(session.WithID("tool-child"))
	m.registerChild(parent, "root", "abcde", "worker", child)
	// A configured store does not make admission durable if its row is absent.
	m.r.sessionStore = session.NewInMemorySessionStore()
	call := tools.ToolCall{ID: "stable-tool-call", Function: tools.FunctionCall{Arguments: `{"to":"abcde","message":"update","delivery_mode":"new_turn"}`}}
	for i := range 2 {
		result, err := m.r.handleSendMessage(t.Context(), parent, call, nil, tools.NopRuntime{})
		require.NoError(t, err)
		require.False(t, result.IsError, result.Output)
		var receipt subagent.DeliveryReceipt
		require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
		assert.True(t, receipt.Accepted)
		assert.False(t, receipt.Durable)
		assert.Equal(t, i == 1, receipt.Idempotent)
		assert.Equal(t, "agent:tool-parent:stable-tool-call", receipt.RequestID)
	}
	d, _ := m.r.sessionDrivers.Lookup(child.ID)
	require.Len(t, d.pending, 1)
	assert.Equal(t, "turn", d.pending[0].InputMode)
}

func TestCommunicationRejectedGuidanceRemainsRetriable(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("capacity-parent"))
	child := session.New(session.WithID("capacity-child"))
	m.registerChild(parent, "root", "abcde", "worker", child)
	d, _ := m.r.sessionDrivers.Lookup(child.ID)
	m.r.maxPendingMailbox = 1
	first := agentCommunication("first", "first", parent.ID, "root", subagent.DeliveryNewTurn)
	_, err := d.postCommunication(t.Context(), first)
	require.NoError(t, err)
	guidance := agentCommunication("guidance", "retry-guidance", parent.ID, "root", subagent.DeliveryGuidance)
	rejected, err := d.postCommunication(t.Context(), guidance)
	require.Error(t, err)
	assert.Equal(t, "capacity", communicationRejection(rejected, err).Rejection)
	assert.False(t, rejected.Accepted)
	assert.Empty(t, d.drainBoundarySteering())
	m.r.maxPendingMailbox = 2
	accepted, err := d.postCommunication(t.Context(), guidance)
	require.NoError(t, err)
	assert.True(t, accepted.Accepted)
	assert.False(t, accepted.Idempotent)
	require.Len(t, d.pending, 2)
}

func TestCommunicationExplicitMessagesAndCompletionHaveIndependentIdentity(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("reply-parent"))
	child := session.New(session.WithID("reply-child"))
	m.registerChild(parent, "root", "36c92-full", "engineer", child)
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("engineer", "engineer")))
	m.r.agents = newAgentRouter(m.r.team, "root")
	m.children["36c92-full"].durable.Result = "same body"
	m.reportTurn(t, "36c92-full", subagent.NodeIdle, "")
	child.AgentName = "engineer"
	d, ok := m.r.sessionDrivers.Lookup(parent.ID)
	require.True(t, ok)
	for _, id := range []string{"explicit-one", "explicit-two", "explicit-three"} {
		call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Arguments: `{"to":"parent","message":"same body","request_id":"` + id + `"}`}}
		for retry := range 2 {
			result, err := m.r.handleSendMessage(t.Context(), child, call, nil, tools.NopRuntime{})
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			var receipt subagent.DeliveryReceipt
			require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
			assert.True(t, receipt.Accepted)
			assert.True(t, receipt.Durable)
			assert.Equal(t, retry == 1, receipt.Idempotent)
			assert.Equal(t, id, receipt.RequestID)
		}
	}
	childDriver, ok := m.r.sessionDrivers.Lookup(child.ID)
	require.True(t, ok)
	require.NoError(t, m.completeSessionTurn(childDriver, m.children["36c92-full"].durable.LastTurnID, ""))
	assert.False(t, m.r.sessionDrivers.deliverReports(d))
	messages := d.session().MessagesSnapshot()
	require.Len(t, messages, 4)
	assert.Equal(t, session.InputOriginRuntime, messages[0].Message.InputOrigin)
	assert.Equal(t, session.ReportOutcomeFinished, messages[0].Message.ReportOutcome)
	assert.Equal(t, child.ID, messages[0].Message.SenderID)
	ids := make(map[string]bool)
	for i, item := range messages {
		message := item.Message
		require.NotNil(t, message)
		require.NotEmpty(t, message.TurnID)
		assert.False(t, ids[message.TurnID])
		ids[message.TurnID] = true
		if i > 0 {
			assert.Equal(t, session.InputOriginAgent, message.InputOrigin)
			assert.Empty(t, message.ReportOutcome)
			assert.Equal(t, "36c92-full", message.SenderID)
			assert.Equal(t, "same body", message.Message.Content)
		}
	}
	require.Len(t, d.DrainRuntimeNotes(), 4)
	assert.Empty(t, d.DrainRuntimeNotes())
	stored, err := m.r.sessionStore.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	storedIDs := make(map[string]bool)
	for _, item := range stored.MessagesSnapshot() {
		if item.Message == nil {
			continue
		}
		assert.False(t, item.Message.Pending)
		assert.False(t, storedIDs[item.Message.TurnID])
		storedIDs[item.Message.TurnID] = true
	}
	assert.Equal(t, ids, storedIDs)
}

func TestTreeMessageGrantAllowsSameRootSiblingAndDeniesCrossRoot(t *testing.T) {
	m := newTestSubagentManager(t)
	rootA := session.New(session.WithID("message-root-a"))
	rootB := session.New(session.WithID("message-root-b"))
	sender := session.New(session.WithID("message-sender"), session.WithAgentName("worker"))
	target := session.New(session.WithID("message-target"), session.WithAgentName("worker"))
	foreign := session.New(session.WithID("message-foreign"), session.WithAgentName("worker"))
	m.registerChild(rootA, "root", "a0001", "worker", sender)
	m.registerChild(rootA, "root", "a0002", "worker", target)
	m.registerChild(rootB, "root", "b0001", "worker", foreign)
	m.mu.Lock()
	m.children["a0001"].durable = session.ChildRecord{RootSessionID: rootA.ID, ParentSessionID: rootA.ID, Node: subagent.Node{ID: "a0001", Agent: "worker", Parent: subagent.SessionRootID(rootA.ID), SessionID: sender.ID, State: subagent.NodeIdle}}
	m.children["a0002"].parentSession = sender.ID
	m.children["a0002"].durable = session.ChildRecord{RootSessionID: rootA.ID, ParentSessionID: sender.ID, Node: subagent.Node{ID: "a0002", Agent: "worker", Parent: "a0001", SessionID: target.ID, State: subagent.NodeIdle}}
	m.children["b0001"].durable = session.ChildRecord{RootSessionID: rootB.ID, ParentSessionID: rootB.ID, Node: subagent.Node{ID: "b0001", Agent: "worker", Parent: subagent.SessionRootID(rootB.ID), SessionID: foreign.ID, State: subagent.NodeIdle}}
	m.mu.Unlock()
	access := &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("worker", "worker")), team.WithAgentConfigs(map[string]latest.AgentConfig{"worker": {Name: "worker", SubagentAccess: access}}))
	m.r.agents = newAgentRouter(m.r.team, "root")
	call := func(target string) (*tools.ToolCallResult, error) {
		return m.r.handleSendMessage(t.Context(), sender, tools.ToolCall{ID: "send-" + target, Function: tools.FunctionCall{Arguments: `{"to":"` + target + `","message":"hello"}`}}, nil, tools.NopRuntime{})
	}
	result, err := call("a0002")
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	var receipt subagent.DeliveryReceipt
	require.NoError(t, json.Unmarshal([]byte(result.Output), &receipt))
	assert.True(t, receipt.Accepted)
	foreignResult, err := call("b0001")
	require.NoError(t, err)
	require.True(t, foreignResult.IsError)
	require.NoError(t, json.Unmarshal([]byte(foreignResult.Output), &receipt))
	assert.Equal(t, "invalid", receipt.Rejection)
}

func TestTreeMessageRejectsForgedParentAndStoppedTarget(t *testing.T) {
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("forged-parent"))
	caller := session.New(session.WithID("forged-caller"), session.WithParentID(parent.ID), session.WithAgentName("worker"))
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("worker", "worker")), team.WithAgentConfigs(map[string]latest.AgentConfig{"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}}}))
	m.r.agents = newAgentRouter(m.r.team, "root")
	result, err := m.r.handleSendMessage(t.Context(), caller, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"to":"parent","message":"should not pass"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError, "caller-supplied ParentID is not runtime membership authority")

	root := session.New(session.WithID("stopped-root"))
	child := session.New(session.WithID("stopped-child"), session.WithAgentName("worker"))
	m.registerChild(root, "root", "c0001", "worker", child)
	m.mu.Lock()
	m.children["c0001"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "c0001", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, State: subagent.NodeStopped}}
	m.mu.Unlock()
	result, err = m.r.handleSendMessage(t.Context(), root, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"to":"c0001","message":"no"}`}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError, "stopped members remain visible but cannot receive messages")
}

func TestTreeMessageRaceStopRoot(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("race-stop-root"))
	sender := session.New(session.WithID("race-stop-sender"), session.WithAgentName("worker"))
	target := session.New(session.WithID("race-stop-target"), session.WithAgentName("worker"))
	m.registerChild(root, "root", "d0001", "worker", sender)
	m.registerChild(root, "root", "d0002", "worker", target)
	m.mu.Lock()
	m.children["d0001"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "d0001", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: sender.ID, State: subagent.NodeIdle}}
	m.children["d0002"].durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "d0002", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: target.ID, State: subagent.NodeIdle}}
	m.mu.Unlock()
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("worker", "worker")), team.WithAgentConfigs(map[string]latest.AgentConfig{"worker": {Name: "worker", SubagentAccess: &latest.SubagentAccessConfig{SendMessage: latest.SubagentAccessTree}}}))
	m.r.agents = newAgentRouter(m.r.team, "root")
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 10 {
				_, _ = m.sendCommunicationToTree(t.Context(), sender.ID, "d0002", "message", fmt.Sprintf("race-%d", i), subagent.DeliveryGuidance, "worker")
			}
		})
	}
	m.mu.Lock()
	if m.stopping == nil {
		m.stopping = map[string]bool{}
	}
	m.stopping[root.ID] = true
	m.mu.Unlock()
	wg.Wait()
	_, err := m.sendCommunicationToTree(t.Context(), sender.ID, subagent.SessionRootID(root.ID), "after-stop", "after-stop", subagent.DeliveryGuidance, "worker")
	require.Error(t, err, "stopping root messaging must be rejected at the boundary")
}
