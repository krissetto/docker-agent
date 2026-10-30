package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func subagentEvent(sessionID string, status facade.SubagentStatus) facade.Event {
	return facade.Event{SessionID: sessionID, Payload: status}
}

func TestBackgroundStatusFromEventMapsStatesAndGuardsChildKey(t *testing.T) {
	t.Parallel()
	const root = "root-session"
	running := map[string]bool{"starting": true, "running": true, "idle": false, "completed": false, "failed": false, "stopped": false}
	for state, want := range running {
		status, ok := backgroundStatusFromEvent(root, subagentEvent(root, facade.SubagentStatus{ChildSessionID: "child", Agent: "reviewer", State: state}))
		require.True(t, ok, state)
		assert.Equal(t, messages.BackgroundAgentStatusMsg{ChildSessionID: "child", AgentName: "reviewer", Running: want}, status, state)
	}

	_, ok := backgroundStatusFromEvent(root, subagentEvent(root, facade.SubagentStatus{ChildSessionID: "", State: "running"}))
	assert.False(t, ok, "a status without a child key is dropped")
	_, ok = backgroundStatusFromEvent(root, subagentEvent(root, facade.SubagentStatus{ChildSessionID: root, State: "running"}))
	assert.False(t, ok, "the root never counts as its own background agent")
	_, ok = backgroundStatusFromEvent(root, facade.Event{SessionID: root, Payload: facade.QueueAccepted{}})
	assert.False(t, ok, "other payloads are not background status")
}

// fakeBackgroundFeed is a subscription whose channel is already closed, so the
// reader never blocks in tests; close counts the release.
func fakeBackgroundFeed(closes *int) *backgroundStatusFeed {
	events := make(chan facade.Event)
	close(events)
	return &backgroundStatusFeed{rootID: "root", sub: facade.Subscription{Events: events, Close: func() { *closes++ }}}
}

func TestBackgroundStatusEventRoutesToItsTabOnly(t *testing.T) {
	t.Parallel()
	m, _ := newPlansTestModel(t)
	var closes int
	feed := fakeBackgroundFeed(&closes)
	m.chatPages["tab"] = &planSidebarTestPage{}
	m.backgroundStatusFeeds = map[string]*backgroundStatusFeed{"tab": feed}

	_, cmd := m.Update(backgroundStatusEventMsg{tabID: "tab", feed: feed, event: subagentEvent("root", facade.SubagentStatus{ChildSessionID: "child", Agent: "reviewer", State: "running"}), ok: true})
	routed, ok := firstOfType[messages.RoutedMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, "tab", routed.SessionID)
	assert.Equal(t, messages.BackgroundAgentStatusMsg{ChildSessionID: "child", AgentName: "reviewer", Running: true}, routed.Inner)

	// A feed that is no longer the tab's current one delivers nothing.
	stale := fakeBackgroundFeed(&closes)
	_, cmd = m.Update(backgroundStatusEventMsg{tabID: "tab", feed: stale, event: subagentEvent("root", facade.SubagentStatus{ChildSessionID: "x", State: "running"}), ok: true})
	assert.Nil(t, cmd, "a stale feed is ignored")
}

func TestBackgroundStatusFeedStopsWithItsTabAndWhenItEnds(t *testing.T) {
	t.Parallel()
	m, _ := newPlansTestModel(t)
	var closes int
	feed := fakeBackgroundFeed(&closes)
	m.chatPages["tab"] = &planSidebarTestPage{}
	m.sessionStates["tab"] = service.NewSessionState(session.New())
	m.backgroundStatusFeeds = map[string]*backgroundStatusFeed{"tab": feed}

	m.disposeSessionComponents("tab")
	assert.Equal(t, 1, closes, "disposing a tab releases its subscription")
	assert.Empty(t, m.backgroundStatusFeeds)

	closes = 0
	feed = fakeBackgroundFeed(&closes)
	m.chatPages["tab"] = &planSidebarTestPage{}
	m.backgroundStatusFeeds = map[string]*backgroundStatusFeed{"tab": feed}
	m.Update(backgroundStatusEventMsg{tabID: "tab", feed: feed, ok: false})
	assert.Equal(t, 1, closes, "a subscription that ends releases its feed once")
	assert.Empty(t, m.backgroundStatusFeeds)
	m.closeBackgroundStatusFeed("tab")
	assert.Equal(t, 1, closes, "a second close is a no-op")
}

func TestBackgroundStatusOpenedWithoutTabIsReleased(t *testing.T) {
	t.Parallel()
	m, _ := newPlansTestModel(t)
	var closes int
	feed := fakeBackgroundFeed(&closes)
	cmd := m.handleBackgroundStatusOpened(backgroundStatusOpenedMsg{tabID: "gone", feed: feed})
	assert.Nil(t, cmd)
	assert.Equal(t, 1, closes, "a feed for a tab that no longer exists is released")
	assert.Empty(t, m.backgroundStatusFeeds)
}
