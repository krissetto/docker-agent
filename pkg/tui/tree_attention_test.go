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
	assert.Contains(t, signalText(t, m.observeSessionSignals(m.paneFocus(), reset)), "Recovery uncertain: 2 turns were interrupted")
}
