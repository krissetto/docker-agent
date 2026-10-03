package leantui

import (
	"context"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/subagent"
)

func connectionLabel(state app.ConnectionState) string {
	switch state {
	case app.ConnectionReconnecting, app.ConnectionDisconnected:
		return state.String()
	default:
		return ""
	}
}

// observeTreeAttention announces descendants that newly wait on a human; the
// same portable tree drives in-process and remote views.
func (m *model) observeTreeAttention(snapshot subagent.Snapshot) {
	if m.app == nil || m.app.Session() == nil {
		return
	}
	items := app.DescendantAttention(snapshot, m.app.Session().ID)
	next := make(map[string]string, len(items))
	for _, item := range items {
		next[item.SessionID] = item.WaitingOn
		if m.treeAttention[item.SessionID] == item.WaitingOn {
			continue
		}
		name := item.Name
		if name == "" {
			name = "A subagent"
		}
		text := name + " is waiting to " + item.WaitingOn + " · /attention to open it"
		if item.WaitingOn == "failed" {
			text = name + " failed · /attention to open it"
		}
		m.addNotice("⚠ ", text, ui.StWarning())
	}
	m.treeAttention = next
	m.status.Attention = len(items)
}

// openTreeAttention opens the next waiting descendant, approvals first,
// through the canonical session-view route.
func (m *model) openTreeAttention(ctx context.Context) {
	var best *app.TreeAttention
	rank := map[string]int{"approve tool": 0, "answer question": 1}
	score := func(item app.TreeAttention) int {
		if r, ok := rank[item.WaitingOn]; ok {
			return r
		}
		return len(rank)
	}
	for _, item := range m.app.DescendantAttention() {
		if best == nil || score(item) < score(*best) {
			candidate := item
			best = &candidate
		}
	}
	if best == nil {
		m.reportCapability("No subagent is waiting on you.", nil)
		return
	}
	m.attachSubagentViewer(ctx, best.SessionID)
}
