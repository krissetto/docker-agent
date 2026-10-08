package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
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
	msg := agentCommunication("do this", "stable-request", parent.ID, "root", subagent.DeliveryGuidance)
	first, err := d.postCommunication(t.Context(), msg)
	require.NoError(t, err)
	assert.True(t, first.Accepted)
	assert.True(t, first.Durable)
	assert.False(t, first.Idempotent)
	assert.False(t, first.Queued, "guidance to active work enters its steering lane")
	retry, err := d.postCommunication(t.Context(), msg)
	require.NoError(t, err)
	assert.True(t, retry.Idempotent)
	assert.True(t, retry.Durable)
	assert.Equal(t, first.Queued, retry.Queued, "exact retry must preserve the STEERING lane receipt")
	require.Empty(t, d.pending)
	require.Len(t, d.steering, 1)
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
	require.Empty(t, d.pending)
	require.Len(t, d.steering, 1)
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
	assert.False(t, d.hasSteeringLocked())
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
	m.children["36c92-full"].durable.Result = "same body"
	m.reportTurn(t, "36c92-full", subagent.NodeIdle, "")
	m.r.team = team.New(team.WithAgents(agent.New("root", "root"), agent.New("engineer", "engineer")))
	m.r.agents = newAgentRouter(m.r.team, "root")
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
