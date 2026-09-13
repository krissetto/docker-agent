package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// timerRecordingPage scripts a chat.Page for the handleRoutedMsg contract:
// it records the messages it was updated with and hands out canned commands
// for the regular (UI) and routed-timer channels.
type timerRecordingPage struct {
	mockChatPage

	updates  []tea.Msg
	uiCmd    tea.Cmd
	timerCmd tea.Cmd
}

func (p *timerRecordingPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	p.updates = append(p.updates, msg)
	return p, p.uiCmd
}

func (p *timerRecordingPage) VisualGeneration() uint64 { return 0 }

func (p *timerRecordingPage) TakeRoutedTimers() tea.Cmd {
	cmd := p.timerCmd
	p.timerCmd = nil
	return cmd
}

type (
	uiMarkerMsg    struct{}
	timerMarkerMsg struct{}
)

// newRoutedTestModel wires an appModel around a real supervisor holding two
// sessions (the first added is active) with one chat page each, built by
// makePage.
func newRoutedTestModel(t *testing.T, makePage func(sess *session.Session, routingID string) chat.Page) (m *appModel, activeID, backgroundID string) {
	t.Helper()
	m, _ = newTestModel(t)

	sv := supervisor.New(nil)
	sessA, sessB := session.New(), session.New()
	var err error
	activeID, err = sv.AddSession(t.Context(), nil, sessA, "", nil)
	require.NoError(t, err)
	backgroundID, err = sv.AddSession(t.Context(), nil, sessB, "", nil)
	require.NoError(t, err)
	m.supervisor = sv
	require.Equal(t, activeID, sv.ActiveID())

	m.chatPages[activeID] = makePage(sessA, activeID)
	m.chatPages[backgroundID] = makePage(sessB, backgroundID)
	m.chatPage = m.chatPages[activeID]
	m.sessionStates[activeID] = service.NewSessionState(sessA)
	m.sessionStates[backgroundID] = service.NewSessionState(sessB)
	return m, activeID, backgroundID
}

// TestHandleRoutedMsg_InactiveTabKeepsRoutedTimers verifies the routing
// contract for hidden tabs: the inner message is applied to the owning page,
// its UI-only command is discarded, and its routed one-shot timers are the
// command handleRoutedMsg returns — so presentation deadlines keep running
// while the tab is hidden.
func TestHandleRoutedMsg_InactiveTabKeepsRoutedTimers(t *testing.T) {
	t.Parallel()

	m, _, backgroundID := newRoutedTestModel(t, func(*session.Session, string) chat.Page {
		return &timerRecordingPage{
			uiCmd:    func() tea.Msg { return uiMarkerMsg{} },
			timerCmd: func() tea.Msg { return timerMarkerMsg{} },
		}
	})
	background := m.chatPages[backgroundID].(*timerRecordingPage)

	inner := runtime.AgentSwitching(true, "root", "scout")
	_, cmd := m.Update(messages.RoutedMsg{SessionID: backgroundID, Inner: inner})

	require.Len(t, background.updates, 1, "the inner message reaches the owning page")
	assert.Same(t, inner, background.updates[0])

	msgs := collectMsgs(cmd)
	assert.True(t, hasMsg[timerMarkerMsg](msgs), "the page's routed timers are dispatched")
	assert.False(t, hasMsg[uiMarkerMsg](msgs), "UI-only cmds of hidden pages stay discarded")

	// A routed message for an unknown (closed) tab is dropped entirely.
	_, cmd = m.Update(messages.RoutedMsg{SessionID: "gone", Inner: inner})
	assert.Nil(t, cmd)
	assert.Len(t, background.updates, 1)
}

// newRealChatPage builds a real chat page bound to a fresh app around sess,
// with its routing identity set and sized so its sidebar renders. The stream
// cancel on cleanup stops any animations a test started (e.g. a transfer's
// rail), so no registration leaks on the global animation coordinator.
func newRealChatPage(t *testing.T, sess *session.Session, routingID string) chat.Page {
	t.Helper()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	page := chat.New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), ss)
	page.SetRoutingID(routingID)
	_ = page.SetSize(140, 40)
	t.Cleanup(func() {
		_, _ = page.Update(messages.StreamCancelledMsg{})
	})
	return page
}

// TestHandleRoutedMsg_TransferOnHiddenTabArmsTimersAndStaysLocal exercises
// the real pieces end to end: a transfer_task start routed to a hidden tab
// shows the transfer box on that tab only (never on the active one) and
// still arms the presentation timers — the command handleRoutedMsg returns —
// even though the page's regular command is discarded.
func TestHandleRoutedMsg_TransferOnHiddenTabArmsTimersAndStaysLocal(t *testing.T) {
	t.Parallel()

	m, activeID, backgroundID := newRoutedTestModel(t, func(sess *session.Session, routingID string) chat.Page {
		return newRealChatPage(t, sess, routingID)
	})

	const transferBoxMarker = "root ●──► scout"
	_, cmd := m.Update(messages.RoutedMsg{
		SessionID: backgroundID,
		Inner:     runtime.AgentSwitching(true, "root", "scout"),
	})

	assert.NotNil(t, cmd, "the hidden tab's presentation timers survive the UI-cmd discard")
	assert.Contains(t, ansi.Strip(m.chatPages[backgroundID].View()), transferBoxMarker,
		"the hop's box shows on its owning tab")
	assert.NotContains(t, ansi.Strip(m.chatPages[activeID].View()), transferBoxMarker,
		"the active tab shows nothing for another tab's hop")
}

func TestHiddenSidebarUpdatesNeverAcquireFinitePresentationLease(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m, activeID, backgroundID := newRoutedTestModel(t, func(sess *session.Session, routingID string) chat.Page {
		page := chat.New(ar, t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{})), service.NewSessionState(sess))
		page.SetRoutingID(routingID)
		page.SetSize(140, 40)
		return page
	})
	m.ar = ar
	activeBefore := m.ar.ActiveCount()
	_, cmd := m.Update(messages.RoutedMsg{SessionID: backgroundID, Inner: runtime.PendingUserMessageAccepted(backgroundID, "hidden-accepted-turn", "Hidden pending content", nil, 1)})
	require.Equal(t, activeID, m.supervisor.ActiveID())
	require.Equal(t, activeBefore, m.ar.ActiveCount(), "hidden queue/layout admission cannot acquire a discarded tick command")
	require.NotNil(t, m.chatPages[backgroundID])
	page, ok := m.chatPages[backgroundID].(interface{ SidebarView() string })
	require.True(t, ok)
	require.Contains(t, ansi.Strip(page.SidebarView()), "Hidden pending content", "hidden presentation snaps to its current canonical target")
	require.Empty(t, collectMsgs(cmd))
	_, cmd = m.Update(messages.RoutedMsg{SessionID: backgroundID, Inner: runtime.PendingUserMessageCanceled(backgroundID, "hidden-accepted-turn", 1)})
	require.Equal(t, activeBefore, m.ar.ActiveCount())
	require.NotContains(t, ansi.Strip(page.SidebarView()), "Hidden pending content")
	require.Empty(t, collectMsgs(cmd))
}
