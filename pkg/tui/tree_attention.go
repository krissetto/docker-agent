package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// observeSessionSignals surfaces cross-cutting session facts (transport state,
// recovery uncertainty and descendant attention) once per routed owner,
// whether its view is focused, backgrounded or remote.
func (m *appModel) observeSessionSignals(owner string, inner tea.Msg) tea.Cmd {
	var event runtime.Event
	switch msg := inner.(type) {
	case messages.SessionRuntimeEventMsg:
		event = msg.Event
	case runtime.Event:
		event = msg
	}
	switch e := event.(type) {
	case *app.ConnectionStateEvent:
		if owner != m.paneFocus() {
			return nil
		}
		switch e.State {
		case app.ConnectionReconnecting:
			return notification.WarningCmd("Session connection lost; reconnecting. Accepted work continues on the server.")
		case app.ConnectionConnected:
			return notification.SuccessCmd("Session connection restored")
		case app.ConnectionDisconnected:
			return notification.ErrorCmd("Session connection closed; accepted work continues on the server. Reopen the session to reattach.")
		}
	case *app.SessionResetEvent:
		if n := e.Snapshot.Status.InterruptedTurns; n > 0 && owner == m.paneFocus() {
			return notification.WarningCmd(lifecycle.InterruptedTurnsNotice(n))
		}
	case *runtime.SubagentTreeEvent:
		return m.observeTreeAttention(owner, e.Snapshot)
	}
	return nil
}

// observeTreeAttention announces each descendant that newly needs a human,
// deduplicated across nested views sharing one tree.
func (m *appModel) observeTreeAttention(owner string, snapshot subagent.Snapshot) tea.Cmd {
	runner := m.supervisor.GetRunner(owner)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return nil
	}
	before := m.treeAttentionUnion()
	items := app.DescendantAttention(snapshot, runner.App.Session().ID)
	if m.treeAttention == nil {
		m.treeAttention = make(map[string][]app.TreeAttention)
	}
	if len(items) == 0 {
		delete(m.treeAttention, owner)
	} else {
		m.treeAttention[owner] = items
	}
	focused := ""
	if focus := m.supervisor.GetRunner(m.paneFocus()); focus != nil && focus.App != nil && focus.App.Session() != nil {
		focused = focus.App.Session().ID
	}
	var cmds []tea.Cmd
	for _, item := range items {
		if item.OccurrenceID != "" {
			if m.treeAttentionSeen == nil {
				m.treeAttentionSeen = make(map[string]struct{})
			}
			key := item.SessionID + "\x00" + item.OccurrenceID
			_, seen := m.treeAttentionSeen[key]
			m.treeAttentionSeen[key] = struct{}{}
			if seen || item.SessionID == focused {
				continue
			}
		} else if before[item.SessionID] == attentionIdentity(item) || item.SessionID == focused {
			continue
		}
		before[item.SessionID] = attentionIdentity(item)
		cmds = append(cmds, notification.WarningCmd(attentionNotice(item)))
	}
	return tea.Batch(cmds...)
}

func attentionIdentity(item app.TreeAttention) string {
	if item.OccurrenceID != "" {
		return item.OccurrenceID
	}
	return item.WaitingOn + "\x00" + item.Error
}

func attentionNotice(item app.TreeAttention) string {
	name := item.Name
	if name == "" {
		name = "A subagent"
	}
	if item.WaitingOn == "failed" {
		return name + " failed: " + chat.TruncateUTF8Bytes(item.Error, 300) + " · /attention to open it"
	}
	return name + " is waiting to " + item.WaitingOn + " · /attention to open it"
}

func (m *appModel) treeAttentionUnion() map[string]string {
	union := make(map[string]string)
	for owner, items := range m.treeAttention {
		if m.supervisor.GetRunner(owner) == nil {
			delete(m.treeAttention, owner)
			continue
		}
		for _, item := range items {
			union[item.SessionID] = attentionIdentity(item)
		}
	}
	return union
}

// openTreeAttention opens the next waiting descendant through the same
// canonical session-view route as tree navigation.
func (m *appModel) openTreeAttention() (tea.Model, tea.Cmd) {
	next, ok := m.nextTreeAttention()
	if !ok {
		return m, notification.InfoCmd("No subagent is waiting on you")
	}
	if next.WaitingOn == "failed" && m.application != nil && m.application.Session() != nil && next.SessionID == m.application.Session().ID {
		return m, notification.ErrorCmd(attentionFailure(next))
	}
	if open := m.supervisor.FindBySession(next.SessionID); open != nil {
		model, cmd := m.handleSwitchTab(open.ID)
		if next.WaitingOn == "failed" {
			return model, tea.Batch(cmd, notification.ErrorCmd(attentionFailure(next)))
		}
		return model, cmd
	}
	cmd := m.beginSubagentOpening(next.NodeID, next.SessionID, next.Name, next.Agent)
	if next.WaitingOn == "failed" {
		cmd = tea.Batch(cmd, notification.ErrorCmd(attentionFailure(next)))
	}
	return m, cmd
}

func attentionFailure(item app.TreeAttention) string {
	name := item.Name
	if name == "" {
		name = "Subagent"
	}
	return name + " failed: " + chat.TruncateUTF8Bytes(item.Error, 300) + ". Send a follow-up to retry or change the task."
}

// nextTreeAttention picks approvals before answers, then failures.
func (m *appModel) nextTreeAttention() (app.TreeAttention, bool) {
	focused := ""
	if focus := m.supervisor.GetRunner(m.paneFocus()); focus != nil && focus.App != nil && focus.App.Session() != nil {
		focused = focus.App.Session().ID
	}
	rank := func(item app.TreeAttention) int {
		switch item.WaitingOn {
		case "approve tool":
			return 0
		case "answer question":
			return 1
		default:
			return 2
		}
	}
	var best *app.TreeAttention
	tabs, _ := m.supervisor.GetTabs()
	for _, tab := range tabs {
		for _, item := range m.treeAttention[tab.SessionID] {
			if (item.SessionID != focused || item.WaitingOn == "failed") && (best == nil || rank(item) < rank(*best)) {
				candidate := item
				best = &candidate
			}
		}
	}
	if best == nil {
		return app.TreeAttention{}, false
	}
	return *best, true
}
