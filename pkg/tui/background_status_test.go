package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
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

// lazyRestoreFixture builds n tabs with loaded sessions. pending marks tabs
// whose session is still lazy, as restored tabs are until their first switch.
func lazyRestoreFixture(t *testing.T, n int, pending []string) (*appModel, []string, *int) {
	t.Helper()
	m, _ := newPlansTestModel(t)
	sv := supervisor.New(nil)
	var tabs []string
	m.supervisor = sv
	m.pendingRestores = map[string]string{}
	for range n {
		sess := session.New()
		tabID, err := sv.AddSession(t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), sess, "", nil)
		require.NoError(t, err)
		tabs = append(tabs, tabID)
		m.chatPages[tabID] = &planSidebarTestPage{}
	}
	for _, tab := range pending {
		m.pendingRestores[tab] = "saved-" + tab
	}
	var opens int
	m.backgroundStatusOpen = func(string) tea.Cmd {
		opens++
		return func() tea.Msg { return nil }
	}
	return m, tabs, &opens
}

func TestBackgroundStatusOpensNoLazyRestoredTabs(t *testing.T) {
	t.Parallel()
	m, tabs, opens := lazyRestoreFixture(t, 5, nil)
	for _, tab := range tabs {
		m.pendingRestores[tab] = "saved-" + tab
	}
	cmd := m.ensureBackgroundStatusFeeds()
	assert.Nil(t, cmd, "no restored tab subscribes before its first activation")
	assert.Zero(t, *opens, "opening many restored tabs subscribes none")
	assert.Empty(t, m.backgroundStatusPending)
}

func TestBackgroundStatusFirstActivationSubscribesOnce(t *testing.T) {
	t.Parallel()
	m, tabs, opens := lazyRestoreFixture(t, 3, nil)
	for _, tab := range tabs {
		m.pendingRestores[tab] = "saved-" + tab
	}
	m.ensureBackgroundStatusFeeds()
	require.Zero(t, *opens)

	// The first activation finishes loading the session and clears its marker.
	first := tabs[1]
	delete(m.pendingRestores, first)
	m.ensureBackgroundStatusFeeds()
	assert.Equal(t, 1, *opens, "the activated tab subscribes")
	m.ensureBackgroundStatusFeeds()
	assert.Equal(t, 1, *opens, "a repeated ensure does not subscribe again while the open is pending")
	assert.True(t, m.backgroundStatusPending[first])
	assert.False(t, m.backgroundStatusPending[tabs[0]], "the other lazy tabs stay unopened")
}

func TestBackgroundStatusDisposalClosesFeedOnly(t *testing.T) {
	t.Parallel()
	m, tabs, _ := lazyRestoreFixture(t, 2, nil)
	var closes int
	feed := fakeBackgroundFeed(&closes)
	m.backgroundStatusFeeds = map[string]*backgroundStatusFeed{tabs[0]: feed}

	m.disposeSessionComponents(tabs[0])
	assert.Equal(t, 1, closes, "disposal closes the subscription")
	assert.Empty(t, m.backgroundStatusFeeds)
	assert.NotNil(t, m.supervisor.GetRunner(tabs[0]), "disposing the components leaves the runtime session supervised")
}
