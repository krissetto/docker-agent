package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	chatpage "github.com/docker/docker-agent/pkg/tui/page/chat"
)

type rawBackgroundEventPage struct {
	chatpage.Page

	events []*runtime.BackgroundProcessesEvent
}

func (p *rawBackgroundEventPage) SidebarView() string {
	if sidebar, ok := p.Page.(interface{ SidebarView() string }); ok {
		return sidebar.SidebarView()
	}
	return p.View()
}

func (p *rawBackgroundEventPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if bridged, ok := msg.(messages.SessionRuntimeEventMsg); ok {
		if event, ok := bridged.Event.(*runtime.BackgroundProcessesEvent); ok {
			clonedEvent := *event
			clonedEvent.Processes = append([]tools.BackgroundProcess(nil), event.Processes...)
			p.events = append(p.events, &clonedEvent)
		}
	}
	updated, cmd := p.Page.Update(msg)
	p.Page = updated.(chatpage.Page)
	return p, cmd
}

func TestBackgroundProcessEventsRouteToTheirOwnParkedAndChildPages(t *testing.T) {
	rootSession := session.New(session.WithID("background-root-owner"), session.WithAgentName("root"))
	rootApp := app.New(t.Context(), nil, rootSession, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, rootApp)
	root.chatPage.SetLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarRight})
	root.handleWindowResize(160, 48)
	_, _ = root.updateWithLifecycle(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: rootSession.Clone(), Status: runtime.SessionStatus{SessionID: rootSession.ID}}})
	rootBase := &rawBackgroundEventPage{Page: root.chatPage}
	root.chatPage = rootBase
	root.chatPages[rootSession.ID] = rootBase

	childSession := session.New(session.WithID("background-child-owner"), session.WithAgentName("worker"), session.WithParentID(rootSession.ID))
	childApp := app.New(t.Context(), nil, childSession, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), childApp, childSession, "", nil)
	require.NoError(t, err)
	_, _ = root.handleSwitchTab(childSession.ID)
	root.chatPage.SetLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarRight})
	_, _ = root.updateWithLifecycle(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: childSession.Clone(), Status: runtime.SessionStatus{SessionID: childSession.ID}}})
	childBase := &rawBackgroundEventPage{Page: root.chatPage}
	root.chatPage = childBase
	root.chatPages[childSession.ID] = childBase

	childProcess := backgroundProcessForTest("child-process", childSession.ID, "child-call", "CHILD")
	_, _ = root.updateWithLifecycle(messages.SessionRuntimeEventMsg{Event: &runtime.BackgroundProcessesEvent{SessionID: childSession.ID, Processes: []tools.BackgroundProcess{childProcess}}})
	require.Len(t, childBase.events, 1)
	require.Equal(t, childSession.ID, childBase.events[0].SessionID)
	require.Equal(t, []tools.BackgroundProcess{childProcess}, childBase.events[0].Processes)

	_, _ = root.handleSwitchTab(rootSession.ID)
	root.chatPage.SetLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarRight})
	rootProcess := backgroundProcessForTest("root-process", rootSession.ID, "root-call", "ROOT")
	_, _ = root.updateWithLifecycle(messages.SessionRuntimeEventMsg{Event: &runtime.BackgroundProcessesEvent{SessionID: rootSession.ID, Processes: []tools.BackgroundProcess{rootProcess}}})
	require.Len(t, rootBase.events, 1)
	require.Equal(t, rootSession.ID, rootBase.events[0].SessionID)
	require.Equal(t, []tools.BackgroundProcess{rootProcess}, rootBase.events[0].Processes)

	// The parked root still updates only its own page when a child is active.
	updated := backgroundProcessForTest("root-later", rootSession.ID, "later-call", "ROOT-LATER")
	_, _ = root.handleSwitchTab(childSession.ID)
	_, _ = root.deliverRoutedMsg(messages.RoutedMsg{SessionID: rootSession.ID, Inner: messages.SessionRuntimeEventMsg{Event: &runtime.BackgroundProcessesEvent{SessionID: rootSession.ID, Processes: []tools.BackgroundProcess{updated}}}})
	require.Len(t, rootBase.events, 2)
	require.Equal(t, rootSession.ID, rootBase.events[1].SessionID)
	require.Equal(t, []tools.BackgroundProcess{updated}, rootBase.events[1].Processes)
	require.Len(t, childBase.events, 1, "parked child receives no root process event")
}

func TestBackgroundProcessEventsRejectForeignOwnerPayloads(t *testing.T) {
	sess := session.New(session.WithID("background-owner"), session.WithAgentName("root"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, a)
	root.chatPage.SetLayoutSettings(messages.LayoutSettings{SidebarPosition: messages.SidebarRight})
	root.handleWindowResize(160, 48)
	_, _ = root.updateWithLifecycle(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess.Clone(), Status: runtime.SessionStatus{SessionID: sess.ID}}})
	page := &rawBackgroundEventPage{Page: root.chatPage}
	root.chatPage = page
	root.chatPages[sess.ID] = page
	foreign := backgroundProcessForTest("foreign", "another-session", "other-call", "FOREIGN-PROCESS")
	_, _ = root.updateWithLifecycle(messages.SessionRuntimeEventMsg{Event: &runtime.BackgroundProcessesEvent{SessionID: "another-session", Processes: []tools.BackgroundProcess{foreign}}})
	require.Len(t, page.events, 1, "envelope is delivered to the owning page")
	require.Equal(t, "another-session", page.events[0].SessionID)
	require.NotContains(t, ansi.Strip(page.SidebarView()), "FOREIGN-PROCESS", "foreign owner data must not appear in the real sidebar")
}

func backgroundProcessForTest(id, owner, call, command string) tools.BackgroundProcess {
	return tools.BackgroundProcess{ID: id, SessionID: owner, ToolCallID: call, Command: command, State: tools.BackgroundProcessRunning}
}
