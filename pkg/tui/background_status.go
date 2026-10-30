package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/runtime/facade/handlefacade"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// backgroundStatusFeed is one tab's facade background subscription, bound to
// that tab's root session. Its events reach only that tab's page.
type backgroundStatusFeed struct {
	rootID string
	host   facade.Host
	sess   facade.Session
	sub    facade.Subscription
	closed bool
}

// close releases the subscription and its facade session port. The runtime
// session keeps running: the facade only closes its own feed.
func (f *backgroundStatusFeed) close() {
	if f == nil || f.closed {
		return
	}
	f.closed = true
	if f.sub.Close != nil {
		f.sub.Close()
	}
	ctx := context.Background()
	if f.sess != nil {
		_ = f.sess.Close(ctx)
	}
	if f.host != nil {
		_ = f.host.Close(ctx)
	}
}

type backgroundStatusOpenedMsg struct {
	tabID string
	feed  *backgroundStatusFeed
}

type backgroundStatusEventMsg struct {
	tabID string
	feed  *backgroundStatusFeed
	event facade.Event
	ok    bool
}

// backgroundStateRunning reports whether a subagent lifecycle state counts as a
// running background agent. Idle and terminal states do not.
func backgroundStateRunning(state string) bool {
	return state == string(subagent.NodeStarting) || state == string(subagent.NodeRunning)
}

// backgroundStatusFromEvent maps one facade event from a root's background
// subscription to the sidebar message. The child session ID is the key. A
// status without a child, or one that names the root itself, is not a
// background agent and is dropped.
func backgroundStatusFromEvent(rootID string, ev facade.Event) (messages.BackgroundAgentStatusMsg, bool) {
	status, ok := ev.Payload.(facade.SubagentStatus)
	if !ok || status.ChildSessionID == "" || status.ChildSessionID == rootID {
		return messages.BackgroundAgentStatusMsg{}, false
	}
	return messages.BackgroundAgentStatusMsg{
		ChildSessionID: status.ChildSessionID,
		AgentName:      status.Agent,
		Running:        backgroundStateRunning(status.State),
	}, true
}

// openBackgroundStatusCmd opens a root-bound background subscription off the
// update loop. Open reuses the tab's live runtime session; the subscription's
// context is the app lifetime, and it stops when the feed is closed.
func (m *appModel) openBackgroundStatusCmd(tabID string) tea.Cmd {
	if m.supervisor == nil {
		return nil
	}
	runner := m.supervisor.GetRunner(tabID)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return nil
	}
	rootID := runner.App.Session().ID
	rt := runner.App.SessionRuntime()
	ctx := m.ctx()
	return func() tea.Msg {
		host, err := handlefacade.NewHost(rt)
		if err != nil {
			return backgroundStatusOpenedMsg{tabID: tabID}
		}
		sess, err := host.Open(ctx, rootID)
		if err != nil {
			_ = host.Close(ctx)
			return backgroundStatusOpenedMsg{tabID: tabID}
		}
		background, ok := sess.Background()
		if !ok {
			_ = sess.Close(ctx)
			_ = host.Close(ctx)
			return backgroundStatusOpenedMsg{tabID: tabID}
		}
		sub, err := background.Subscribe(ctx)
		if err != nil {
			_ = sess.Close(ctx)
			_ = host.Close(ctx)
			return backgroundStatusOpenedMsg{tabID: tabID}
		}
		return backgroundStatusOpenedMsg{tabID: tabID, feed: &backgroundStatusFeed{rootID: rootID, host: host, sess: sess, sub: sub}}
	}
}

// readBackgroundStatusCmd waits for the feed's next event. A closed channel
// ends the feed.
func readBackgroundStatusCmd(tabID string, feed *backgroundStatusFeed) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-feed.sub.Events
		return backgroundStatusEventMsg{tabID: tabID, feed: feed, event: ev, ok: ok}
	}
}

// ensureBackgroundStatusFeeds opens a feed for every session tab that lacks
// one. Opening is idempotent per tab while its open is pending.
func (m *appModel) ensureBackgroundStatusFeeds() tea.Cmd {
	if m.backgroundStatusFeeds == nil {
		m.backgroundStatusFeeds = make(map[string]*backgroundStatusFeed)
	}
	if m.backgroundStatusPending == nil {
		m.backgroundStatusPending = make(map[string]bool)
	}
	var cmds []tea.Cmd
	for tabID := range m.chatPages {
		if m.backgroundStatusFeeds[tabID] != nil || m.backgroundStatusPending[tabID] {
			continue
		}
		if cmd := m.openBackgroundStatusCmd(tabID); cmd != nil {
			m.backgroundStatusPending[tabID] = true
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// closeBackgroundStatusFeed stops a tab's feed when its components are disposed.
func (m *appModel) closeBackgroundStatusFeed(tabID string) {
	delete(m.backgroundStatusPending, tabID)
	if feed := m.backgroundStatusFeeds[tabID]; feed != nil {
		feed.close()
		delete(m.backgroundStatusFeeds, tabID)
	}
}

// handleBackgroundStatusOpened stores a freshly opened feed, or releases it when
// its tab is gone or already has one.
func (m *appModel) handleBackgroundStatusOpened(msg backgroundStatusOpenedMsg) tea.Cmd {
	delete(m.backgroundStatusPending, msg.tabID)
	if msg.feed == nil {
		return nil
	}
	if _, ok := m.chatPages[msg.tabID]; !ok || m.backgroundStatusFeeds[msg.tabID] != nil {
		msg.feed.close()
		return nil
	}
	m.backgroundStatusFeeds[msg.tabID] = msg.feed
	return readBackgroundStatusCmd(msg.tabID, msg.feed)
}

// handleBackgroundStatusEvent routes one mapped status to its tab and re-arms
// the reader. A stale feed (closed or replaced) is ignored.
func (m *appModel) handleBackgroundStatusEvent(msg backgroundStatusEventMsg) tea.Cmd {
	if m.backgroundStatusFeeds[msg.tabID] != msg.feed || msg.feed == nil {
		return nil
	}
	if !msg.ok {
		m.closeBackgroundStatusFeed(msg.tabID)
		return nil
	}
	cmds := []tea.Cmd{readBackgroundStatusCmd(msg.tabID, msg.feed)}
	if status, ok := backgroundStatusFromEvent(msg.feed.rootID, msg.event); ok {
		cmds = append(cmds, core.CmdHandler(messages.RoutedMsg{SessionID: msg.tabID, Inner: status}))
	}
	return tea.Batch(cmds...)
}
