package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func signalText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	require.NotNil(t, cmd)
	var texts []string
	var collect func(tea.Msg)
	collect = func(msg tea.Msg) {
		switch msg := msg.(type) {
		case notification.ShowMsg:
			texts = append(texts, msg.Text)
		case tea.BatchMsg:
			for _, cmd := range msg {
				if cmd != nil {
					collect(cmd())
				}
			}
		}
	}
	collect(cmd())
	require.Len(t, texts, 1)
	return texts[0]
}

func attentionTree(rootID, waiting string) subagent.Snapshot {
	return subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: subagent.SessionRootID(rootID), SessionID: rootID},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "c0ffe", Parent: subagent.SessionRootID(rootID), SessionID: "child", Name: "Reviewer", NeedsAttention: waiting != "", WaitingOn: waiting}}},
	}}}
}

func TestTreeAttentionNoticeDedupesAndOpensWaitingDescendant(t *testing.T) {
	m := newCloseTabTestModel(t)
	rt := newCloseTabRuntime("root", false)
	addCloseTabTestSession(t, m, "root", rt, nil)
	m.supervisor.SwitchTo("root")

	assert.Equal(t, "Reviewer is waiting to approve tool · /attention to open it", signalText(t, m.observeTreeAttention("root", attentionTree("root", "approve tool"))))
	assert.Nil(t, m.observeTreeAttention("root", attentionTree("root", "approve tool")), "unchanged attention is announced once")

	m.treeAttention["root"] = append(m.treeAttention["root"], app.TreeAttention{SubagentTarget: app.SubagentTarget{SessionID: "asker"}, WaitingOn: "answer question"})
	next, ok := m.nextTreeAttention()
	require.True(t, ok)
	assert.Equal(t, "child", next.SessionID, "approvals are opened before questions")
	assert.Equal(t, subagent.NodeID("c0ffe"), next.NodeID)
	m.treeAttention["root"] = m.treeAttention["root"][:1]

	assert.Nil(t, m.observeTreeAttention("root", attentionTree("root", "")))
	_, cmd := m.openTreeAttention()
	assert.Equal(t, "No subagent is waiting on you", signalText(t, cmd))
}

func TestSessionSignalsReportFocusedConnectionAndRecovery(t *testing.T) {
	m := newCloseTabTestModel(t)
	rt := newCloseTabRuntime("root", false)
	addCloseTabTestSession(t, m, "root", rt, nil)
	addCloseTabTestSession(t, m, "other", rt, nil)
	m.supervisor.SwitchTo("root")

	reconnecting := messages.SessionRuntimeEventMsg{Event: &app.ConnectionStateEvent{State: app.ConnectionReconnecting}}
	assert.Contains(t, signalText(t, m.observeSessionSignals(m.paneFocus(), reconnecting)), "reconnecting")
	assert.Nil(t, m.observeSessionSignals("other", reconnecting), "background views do not stack transport notices")
	restored := messages.SessionRuntimeEventMsg{Event: &app.ConnectionStateEvent{State: app.ConnectionConnected}}
	assert.Equal(t, "Session connection restored", signalText(t, m.observeSessionSignals(m.paneFocus(), restored)))

	reset := messages.SessionRuntimeEventMsg{Event: &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Status: runtime.SessionStatus{SessionID: "root", State: runtime.SessionStateSettled, InterruptedTurns: 2}}}}
	assert.Contains(t, signalText(t, m.observeSessionSignals(m.paneFocus(), reset)), "Recovery uncertain: 2 accepted inputs have no terminal outcome records. Review the transcript before continuing.")
}

func TestTreeAttentionUnknownReasonDoesNotRepeatOnSnapshotRefresh(t *testing.T) {
	m := newCloseTabTestModel(t)
	addCloseTabTestSession(t, m, "root", newCloseTabRuntime("root", false), nil)
	m.supervisor.SwitchTo("root")
	snapshot := attentionTree("root", "")
	node := &snapshot.Nodes[0].Children[0].Node
	node.NeedsAttention = true
	for range 20 {
		assert.Nil(t, m.observeSessionSignals("root", messages.SessionRuntimeEventMsg{Event: runtime.SubagentTree(snapshot)}))
	}
	_, cmd := m.openTreeAttention()
	assert.Equal(t, "No subagent is waiting on you", signalText(t, cmd))
}

func TestTreeAttentionFailureShowsCauseEvenWhenAlreadyFocused(t *testing.T) {
	m := newCloseTabTestModel(t)
	rt := newCloseTabRuntime("root", false)
	addCloseTabTestSession(t, m, "root", rt, nil)
	addCloseTabTestSession(t, m, "child", rt, nil)
	m.supervisor.SwitchTo("root")
	snapshot := attentionTree("root", "failed")
	node := &snapshot.Nodes[0].Children[0].Node
	node.State, node.Error = subagent.NodeFailed, "model unavailable"
	assert.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "model unavailable")
	m.supervisor.SwitchTo("child")
	m.application = m.supervisor.GetRunner("child").App
	_, cmd := m.openTreeAttention()
	assert.Contains(t, signalText(t, cmd), "model unavailable")
}

func TestTreeAttentionDistinctFailuresAndResolvedOccurrences(t *testing.T) {
	m := newCloseTabTestModel(t)
	addCloseTabTestSession(t, m, "root", newCloseTabRuntime("root", false), nil)
	m.supervisor.SwitchTo("root")
	snapshot := attentionTree("root", "failed")
	node := &snapshot.Nodes[0].Children[0].Node
	node.State, node.Error = subagent.NodeFailed, "first failure"
	assert.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "first failure")
	for range 20 {
		assert.Nil(t, m.observeTreeAttention("root", snapshot))
	}
	node.Error = "second failure"
	assert.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "second failure")
	node.State, node.Error, node.NeedsAttention, node.WaitingOn = subagent.NodeRunning, "", false, ""
	assert.Nil(t, m.observeTreeAttention("root", snapshot))
	node.State, node.Error, node.NeedsAttention, node.WaitingOn = subagent.NodeFailed, "second failure", true, "failed"
	assert.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "second failure", "same text after a new turn is a distinct occurrence")
}

func TestTreeAttentionOccurrenceDedupeSurvivesEmptyReconnectAndOwnerRoutes(t *testing.T) {
	m := newCloseTabTestModel(t)
	rt := newCloseTabRuntime("root", false)
	addCloseTabTestSession(t, m, "root", rt, nil)
	addCloseTabTestSession(t, m, "other", rt, nil)
	m.supervisor.SwitchTo("root")
	snapshot := attentionTree("root", "approve tool")
	node := &snapshot.Nodes[0].Children[0].Node
	node.AttentionID = subagent.AttentionIdentity("interaction:first")
	require.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "approve tool")
	node.AttentionID = subagent.AttentionIdentity("interaction:first", "interaction:second")
	require.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "approve tool", "new same-reason ID announces without an empty transition")
	node.AttentionID = subagent.AttentionIdentity("interaction:second")
	require.Nil(t, m.observeTreeAttention("root", snapshot), "resolving one prompt cannot reannounce another")
	require.Nil(t, m.observeTreeAttention("root", attentionTree("root", "")))
	require.Nil(t, m.observeTreeAttention("root", snapshot), "same ID after reconnect/empty snapshot remains observed")
	other := attentionTree("other", "approve tool")
	other.Nodes[0].Children[0].Node.AttentionID = node.AttentionID
	require.Nil(t, m.observeTreeAttention("other", other), "owner routes share occurrence history")
	node.AttentionID = subagent.AttentionIdentity("failure:turn-one")
	node.State, node.Error, node.WaitingOn = subagent.NodeFailed, "same error", "failed"
	require.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "same error")
	node.AttentionID = subagent.AttentionIdentity("failure:turn-two")
	require.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "same error")
	node.AttentionID = subagent.AttentionIdentity("failure:turn-one")
	require.Nil(t, m.observeTreeAttention("root", snapshot), "interleaving old occurrences is not a new notice")
	node.SessionID = "different-child"
	require.Contains(t, signalText(t, m.observeTreeAttention("root", snapshot)), "same error", "different agents remain observable")
}
