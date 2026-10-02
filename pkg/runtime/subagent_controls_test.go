package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestHumanStopSubtreeNestedRetainsHistoryAndRejectsRoot(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"))
	leaf := session.New(session.WithID("leaf"))
	leaf.AddMessage(session.UserMessage("retained transcript"))
	m.registerChild(root, "root", "child-node", "worker", child)
	m.registerChild(child, "worker", "leaf-node", "helper", leaf)
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), root))
	m.coord = store.(session.CoordinationStore)
	for _, id := range []subagent.NodeID{"child-node", "leaf-node"} {
		rec := m.children[id]
		node, ok := m.tree.Node(id)
		require.True(t, ok)
		node.State = subagent.NodeIdle
		rec.durable = session.ChildRecord{RootSessionID: root.ID, ParentSessionID: rec.parentSession, Node: node, Revision: 1}
		row := rec.session.OwnSnapshot()
		row.ParentID = rec.parentSession
		require.NoError(t, m.coord.AdmitChild(t.Context(), session.ChildAdmission{Child: row, Record: rec.durable}))
	}
	m.r.SetUseSubagents(false)
	require.Error(t, m.r.StopSubtree("root:root"))
	require.Error(t, m.r.StopSubtree("missing"))
	require.NoError(t, m.r.StopSubtree("leaf-node"), "human stop resolves the actual parent while delegation is off")
	node, ok := m.tree.Node("child-node")
	require.True(t, ok)
	assert.NotEqual(t, subagent.NodeStopped, node.State)
	node, ok = m.tree.Node("leaf-node")
	require.True(t, ok)
	assert.Equal(t, subagent.NodeStopped, node.State)
	loaded, err := store.GetSession(t.Context(), leaf.ID)
	require.NoError(t, err)
	require.Equal(t, "retained transcript", loaded.Messages[0].Message.Message.Content)
	require.NoError(t, m.r.StopSubtree("child-node"))
	_, err = m.sendToChild(root.ID, "child-node", "more work")
	require.Error(t, err)
}

func TestReportOutcomeMetadataAllLiveTransitionsAndReplay(t *testing.T) {
	for _, outcome := range []session.ReportOutcome{"", session.ReportOutcomeFinished, session.ReportOutcomeFailed} {
		msg := QueuedMessage{InputOrigin: session.InputOriginRuntime, SenderID: "child", SenderName: "worker", InputMode: "steer", ReportOutcome: outcome}
		restored := queuedSessionInput(msg.sessionMessage(), 0, true)
		require.Equal(t, outcome, restored.ReportOutcome)
		events := []Event{&PendingUserMessageAcceptedEvent{}, &PendingUserMessageEditedEvent{}, &PendingUserMessagePromotedEvent{}, &PendingUserMessageCanceledEvent{}, &UserMessageEvent{}}
		for _, event := range events {
			got := inputEventMetadata(event, restored)
			switch got := got.(type) {
			case *PendingUserMessageAcceptedEvent:
				assert.Equal(t, outcome, got.ReportOutcome)
			case *PendingUserMessageEditedEvent:
				assert.Equal(t, outcome, got.ReportOutcome)
			case *PendingUserMessagePromotedEvent:
				assert.Equal(t, outcome, got.ReportOutcome)
			case *PendingUserMessageCanceledEvent:
				assert.Equal(t, outcome, got.ReportOutcome)
			case *UserMessageEvent:
				assert.Equal(t, outcome, got.ReportOutcome)
			}
		}
	}
	assert.Equal(t, session.ReportOutcomeFinished, childReportOutcome(subagent.NodeIdle))
	assert.Equal(t, session.ReportOutcomeFailed, childReportOutcome(subagent.NodeFailed))
}
