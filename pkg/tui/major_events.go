package tui

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type (
	majorEventFence       struct{ epoch, sequence uint64 }
	majorNoticeExpiredMsg struct {
		token    messagebar.Token
		deadline time.Time
	}
)

// Only canonical journal facts count. Seed deliveries advance the high-water
// fence but never create counts; raw stream/tree notifications are not facts.
func (m *appModel) observeMajorEvent(msg messages.SessionRuntimeEventMsg) tea.Cmd {
	owner := msg.OriginSessionID
	if owner == "" || msg.Sequence == 0 || msg.Event == nil {
		return nil
	}
	if m.majorEventWater == nil {
		m.majorEventWater = make(map[string]majorEventFence)
	}
	previous := m.majorEventWater[owner]
	if msg.Epoch < previous.epoch || msg.Sequence <= previous.sequence {
		return nil
	}
	// Retain only journals still represented by a live supervisor owner. This
	// fence is observer state, not closed-session notification history.
	for recorded := range m.majorEventWater {
		if recorded != owner && m.supervisor.FindBySession(recorded) == nil {
			delete(m.majorEventWater, recorded)
		}
	}
	if m.supervisor.FindBySession(owner) == nil {
		return nil
	}
	m.majorEventWater[owner] = majorEventFence{epoch: msg.Epoch, sequence: msg.Sequence}
	if msg.Seed {
		return nil
	}
	event := messagebar.Event{Owner: owner, Sequence: msg.Sequence}
	switch fact := msg.Event.(type) {
	case *runtime.TurnSettledEvent:
		if fact.SessionID != owner || fact.TurnID == "" || msg.TurnID != fact.TurnID {
			return nil
		}
		if m.paneOutcomes == nil {
			m.paneOutcomes = make(map[string]runtime.TurnOutcome)
		}
		m.paneOutcomes[owner] = fact.Outcome
		m.viewCacheValid = false
		if fact.Outcome != runtime.TurnCompleted {
			return nil
		}
		event.Kind, event.ID = messagebar.CompletedTurn, owner+":"+fact.TurnID
	case *runtime.SubagentCreatedEvent:
		if fact.SessionID != owner || fact.ParentSessionID != owner || fact.ChildSessionID == "" || fact.NodeID == "" || fact.CreatedAt.IsZero() {
			return nil
		}
		event.Kind = messagebar.SpawnedSubagent
		event.ID = fmt.Sprintf("%s:%s:%d", fact.NodeID, fact.ChildSessionID, fact.CreatedAt.UnixNano())
	default:
		return nil
	}
	now := time.Now()
	snapshot, changed := m.majorEvents.Add(event, now)
	if !changed {
		return nil
	}
	m.ensureMessageBar()
	name, nodeID := "session", ""
	if runner := m.supervisor.FindBySession(owner); runner != nil {
		name, _ = m.tabAgentIdentity(messages.TabInfo{SessionID: runner.ID})
		if name == "" {
			name = "agent"
		}
		nodeID = majorEventNodeID(runner.App)
	}
	token, cmd, accepted := m.messageBar.SetNotice(messagebar.Message{Owner: owner, AgentName: name, AgentNodeID: nodeID, Text: snapshot.Text, Severity: messagebar.Info, Category: messagebar.Background}, snapshot.Deadline)
	if !accepted {
		return nil
	}
	m.majorNoticeToken = token
	m.majorNoticeDeadline = snapshot.Deadline
	m.viewCacheValid = false
	return tea.Batch(cmd, m.scheduleMajorExpiry(now))
}

// majorEventNodeID follows canonical session ownership, not the focused pane or
// the shape of an ID. A top-level session's synthetic root is not a short node ID.
func majorEventNodeID(application *app.App) string {
	if application == nil || application.Session() == nil {
		return ""
	}
	sess := application.Session()
	rootID := subagent.SessionRootID(sess.ID)
	if attached := application.AttachedSubagent(); attached != nil && attached.NodeID != "" && attached.NodeID != rootID {
		return string(attached.NodeID)
	}
	if lookup, ok := application.Runtime().(subagentSessionLookup); ok {
		if node, found := lookup.SubagentNodeForSession(sess.ID); found && node != "" && node != rootID {
			return string(node)
		}
	}
	if snapshot := sess.GetSubagentTree(); snapshot != nil {
		return majorEventSnapshotNodeID(snapshot.Nodes, sess.ID, rootID)
	}
	return ""
}

func majorEventSnapshotNodeID(nodes []subagent.NodeSnapshot, owner string, rootID subagent.NodeID) string {
	for _, entry := range nodes {
		if entry.Node.SessionID == owner && entry.Node.ID != "" && entry.Node.ID != rootID {
			return string(entry.Node.ID)
		}
		if id := majorEventSnapshotNodeID(entry.Children, owner, rootID); id != "" {
			return id
		}
	}
	return ""
}

func (m *appModel) scheduleMajorExpiry(now time.Time) tea.Cmd {
	if m.majorNoticeTimerPending || m.majorNoticeDeadline.IsZero() {
		return nil
	}
	m.majorNoticeTimerPending = true
	ready := majorNoticeExpiredMsg{token: m.majorNoticeToken, deadline: m.majorNoticeDeadline}
	m.majorScheduledExpiry = ready
	return tea.Tick(max(0, ready.deadline.Sub(now)), func(time.Time) tea.Msg { return ready })
}

func (m *appModel) expireMajorNotice(msg majorNoticeExpiredMsg) tea.Cmd {
	if !m.majorNoticeTimerPending || msg != m.majorScheduledExpiry {
		return nil
	}
	m.majorNoticeTimerPending = false
	if m.messageBar == nil {
		return nil
	}
	now := time.Now()
	if now.Before(m.majorNoticeDeadline) {
		return m.scheduleMajorExpiry(now)
	}
	m.viewCacheValid = false
	m.majorNoticeDeadline = time.Time{}
	return m.messageBar.Expire(m.majorNoticeToken, now)
}
