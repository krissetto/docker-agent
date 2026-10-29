package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func backgroundProcess(id, owner, toolID, command string) tools.BackgroundProcess {
	return tools.BackgroundProcess{ID: id, SessionID: owner, AgentName: "root", ToolCallID: toolID, Command: command, Cwd: "/work", StartedAt: time.Unix(1, 0), State: tools.BackgroundProcessRunning}
}

func newBackgroundProcessPage(t *testing.T, sess *session.Session) *chatPage {
	t.Helper()
	application, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	page := New(ar, t.Context(), application, service.NewSessionState(sess), WithCompactWorkLog(false)).(*chatPage)
	page.SetRoutingID(sess.ID)
	page.SetSize(160, 40)
	page.sidebar.SetCollapsed(false)
	page.sidebar.SetPreferredWidth(36)
	page.SetSize(160, 40)
	page.sidebar.LoadFromSession(sess.Clone())
	if len(sess.Messages) > 0 {
		page.messages.LoadFromSession(sess, nil)
	}
	_ = page.Init()
	t.Cleanup(func() { Cleanup(page); ar.Stop() })
	return page
}

func TestBackgroundProcessesAreLiveOwnerScopedAndClearedByNilSnapshot(t *testing.T) {
	rootSession := session.New(session.WithID("root-owner"), session.WithAgentName("root"))
	childSession := session.New(session.WithID("child-owner"), session.WithAgentName("worker"), session.WithParentID(rootSession.ID))
	rootPage := newBackgroundProcessPage(t, rootSession)
	childPage := newBackgroundProcessPage(t, childSession)
	rootProcess := backgroundProcess("root-process", rootSession.ID, "root-call", "ROOT-ADOPTED-CMD")
	childProcess := backgroundProcess("child-process", childSession.ID, "child-call", "CHILD-ADOPTED-CMD")

	_, _ = rootPage.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: rootSession.ID, Processes: []tools.BackgroundProcess{rootProcess, childProcess}})
	require.Len(t, rootPage.backgroundProcesses, 1)
	require.Equal(t, rootSession.ID, rootPage.backgroundProcesses[0].SessionID)
	rootPage.sidebar.SetMode(sidebar.ModeVertical)
	rootView := ansi.Strip(rootPage.sidebar.View())
	childView := ansi.Strip(childPage.sidebar.View())
	require.Contains(t, rootView, "ROOT-ADOPTED-CMD")
	before := rootPage.backgroundProcessToolCalls
	visualGeneration := rootPage.sidebar.VisualGeneration()
	_, cmd := rootPage.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: rootSession.ID, Processes: []tools.BackgroundProcess{rootProcess, childProcess}})
	require.Nil(t, cmd, "equal authoritative snapshots do not rescan/reseed the sidebar")
	require.Equal(t, before, rootPage.backgroundProcessToolCalls)
	require.Equal(t, visualGeneration, rootPage.sidebar.VisualGeneration(), "equal snapshots must be a no-op")
	require.NotContains(t, rootView, "CHILD-ADOPTED-CMD")
	require.NotContains(t, childView, "ROOT-ADOPTED-CMD")

	_, _ = childPage.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: childSession.ID, Processes: []tools.BackgroundProcess{childProcess}})
	childPage.sidebar.SetMode(sidebar.ModeVertical)
	childView = ansi.Strip(childPage.sidebar.View())
	require.Contains(t, childView, "CHILD-ADOPTED-CMD")
	require.NotContains(t, childView, "ROOT-ADOPTED-CMD")

	_, _ = rootPage.handleRuntimeEvent(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: rootSession.Clone(), Status: runtime.SessionStatus{SessionID: rootSession.ID}, BackgroundProcesses: []tools.BackgroundProcess{rootProcess}}})
	require.Len(t, rootPage.backgroundProcesses, 1, "same-session metadata reseed retains the authoritative process after LoadFromSession clears sidebar state")
	_, _ = rootPage.handleRuntimeEvent(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: rootSession.Clone(), Status: runtime.SessionStatus{SessionID: rootSession.ID}}})
	require.Empty(t, rootPage.backgroundProcesses, "nil process snapshots replace stale state even for an unchanged transcript")
	_, _ = rootPage.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: rootSession.ID, Processes: []tools.BackgroundProcess{childProcess}})
	require.NotContains(t, ansi.Strip(rootPage.sidebar.View()), "CHILD-ADOPTED-CMD", "foreign-only full replacements clear existing rows")
}

func TestBackgroundProcessHistoryDoesNotInventLiveProcesses(t *testing.T) {
	call := tools.ToolCall{ID: "historical-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"HISTORICAL-ONLY"}`}}
	sess := session.New(session.WithID("history-owner"))
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}))
	sess.AddMessage(&session.Message{Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: call.ID, Content: "old result"}})
	page := newBackgroundProcessPage(t, sess)
	view := ansi.Strip(page.sidebar.View())
	require.NotContains(t, view, "Background ·")
	require.NotContains(t, view, "HISTORICAL-ONLY")
}

func TestBackgroundProcessNavigationRequiresUniqueCurrentTranscriptTool(t *testing.T) {
	call := tools.ToolCall{ID: "original-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ORIGINAL-COMMAND"}`}}
	sess := session.New(session.WithID("navigation-owner"))
	sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}))
	page := newBackgroundProcessPage(t, sess)
	process := backgroundProcess("process-id", sess.ID, call.ID, "ORIGINAL-COMMAND")
	_, _ = page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID, Processes: []tools.BackgroundProcess{process}})
	require.True(t, page.messages.HasUniqueToolCall(call.ID))
	require.True(t, page.navigateToBackgroundProcess(call.ID))
	require.False(t, page.navigateToBackgroundProcess("unknown-call"))
	click := backgroundProcessMouseClick(t, page, "ORIGINAL-COMMAND")
	require.Equal(t, TargetSidebarBackgroundProcess, NewHitTest(page).At(click.X, click.Y))
	_, cmd := page.handleMouseClick(click)
	require.Nil(t, cmd, "a safe row navigates locally to its tool call")

	duplicate := session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}})
	sess.AddMessage(duplicate)
	page.messages.LoadFromSession(sess, nil)
	page.backgroundLinkCacheValid = false
	_ = page.refreshBackgroundProcessLinks()
	require.False(t, page.navigateToBackgroundProcess(call.ID))
}

func TestBackgroundProcessRowsWithoutTranscriptMappingAreInformational(t *testing.T) {
	sess := session.New(session.WithID("unmapped-owner"))
	page := newBackgroundProcessPage(t, sess)
	process := backgroundProcess("process-id", sess.ID, "no-original-tool", "UNMAPPED-COMMAND")
	_, _ = page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID, Processes: []tools.BackgroundProcess{process}})
	_, _ = page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: "other-owner", Processes: []tools.BackgroundProcess{{ID: "foreign", SessionID: "other-owner", Command: "FOREIGN-COMMAND", State: tools.BackgroundProcessRunning}}})
	require.NotContains(t, ansi.Strip(page.sidebar.View()), "FOREIGN-COMMAND")
	view := ansi.Strip(page.sidebar.View())
	require.Contains(t, view, "UNMAPPED-COMMAND")
	require.Empty(t, page.backgroundProcessToolCalls)
	_, cmd := page.handleMouseClick(backgroundProcessMouseClick(t, page, "UNMAPPED-COMMAND"))
	require.Nil(t, cmd)
}

func backgroundProcessMouseClick(t *testing.T, page *chatPage, command string) tea.MouseClickMsg {
	t.Helper()
	lines := strings.Split(ansi.Strip(page.sidebar.View()), "\n")
	localY, x := -1, -1
	for i, line := range lines {
		if col := strings.Index(line, command); col >= 0 {
			localY, x = i, ansi.StringWidth(line[:col])+1
			break
		}
	}
	require.GreaterOrEqual(t, localY, 0)
	layout := page.computeSidebarLayout()
	return tea.MouseClickMsg{X: styles.AppPadding + layout.sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft + x, Y: localY, Button: tea.MouseLeft}
}

func TestBackgroundProcessSnapshotsSkipHistoricalTranscriptScans(t *testing.T) {
	sess := session.New(session.WithID("empty-process-owner"))
	page := newBackgroundProcessPage(t, sess)
	before := page.app.Session().ItemCount()
	for i := range 50 {
		call := tools.ToolCall{ID: fmt.Sprintf("history-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"history"}`}}
		page.app.Session().AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}))
	}
	require.NotEqual(t, before, page.app.Session().ItemCount())
	for range 5 {
		_, cmd := page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID})
		require.Nil(t, cmd)
	}
	require.Empty(t, page.backgroundProcesses)
	require.Nil(t, page.backgroundLinkToolCallCounts)
	unchangedGeneration := page.sidebar.VisualGeneration()
	_, cmd := page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: sess.ID, Processes: nil})
	require.Nil(t, cmd)
	require.Equal(t, unchangedGeneration, page.sidebar.VisualGeneration())
}

func TestBackgroundProcessEventRejectsOtherSessionOwner(t *testing.T) {
	sess := session.New(session.WithID("expected-owner"))
	page := newBackgroundProcessPage(t, sess)
	foreign := backgroundProcess("foreign", "other-owner", "foreign-call", "FOREIGN-COMMAND")
	_, _ = page.handleRuntimeEvent(&runtime.BackgroundProcessesEvent{SessionID: foreign.SessionID, Processes: []tools.BackgroundProcess{foreign}})
	require.Empty(t, page.backgroundProcesses)
	require.Empty(t, page.backgroundProcessToolCalls)
}
