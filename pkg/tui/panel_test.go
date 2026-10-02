package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func panelFixture(t *testing.T) *appModel {
	t.Helper()
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.application.Session().WorkingDir = "/workspace/界é-project"
	root.applyPanelSettings(messages.DefaultPanelSettings())
	t.Cleanup(root.panelAnimation.Stop)
	return root
}

func panelTree(id string, state subagent.NodeState) *runtime.SubagentTreeEvent {
	return &runtime.SubagentTreeEvent{Snapshot: subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(id), SessionID: id}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "worker", State: state}}}}}}}
}

func todoEvent(items []session.Todo) *runtime.ToolCallResponseEvent {
	return &runtime.ToolCallResponseEvent{ToolDefinition: tools.Tool{Category: "todo"}, Result: &tools.ToolCallResult{Meta: items}}
}

func TestPanelIndependentCanvasResizeNoticePriorityAndFocus(t *testing.T) {
	root := panelFixture(t)
	for _, hidden := range []bool{false, true} {
		root.hideSidebar = hidden
		root.initSessionComponents("profile", root.application, root.application.Session())
		root.chatPage.Init()
		root.resizeAll()
		root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		row := root.renderMessageBar()
		require.Contains(t, ansi.Strip(row), "界é-project")
		require.Contains(t, ansi.Strip(row), "0 subagents")
		require.Contains(t, ansi.Strip(row), "todos unavailable")
		require.Equal(t, 120, ansi.StringWidth(row))
		for _, cell := range chromeCells(row) {
			require.Nil(t, cell.bg)
		}
	}
	root.focusedPanel = PanelContent
	root.switchFocus()
	require.Equal(t, PanelStatus, root.focusedPanel)
	root.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, messages.PanelSubagents, root.panelFocused)
	_, cmd := root.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, PanelEditor, root.focusedPanel)
	for _, width := range []int{8, 40, 120} {
		root.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		require.Equal(t, width, ansi.StringWidth(root.renderMessageBar()))
	}
	root.Update(messagebar.SetMessageMsg{Text: "Notice", Actions: []messagebar.Action{{Label: "Act", Command: func() tea.Msg { return nil }}}})
	require.Empty(t, root.panelFrame().Zones, "right-edge action retains every cell")
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "Notice")
	root.Update(messagebar.ClearMessageMsg{})
	root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{}})
	require.Empty(t, strings.TrimSpace(ansi.Strip(root.renderMessageBar())))
	require.Empty(t, root.panelFrame().Zones)
}

func TestPanelCanonicalCountsUpdatesImmutabilityAndAnimationLifecycle(t *testing.T) {
	root := panelFixture(t)
	event := panelTree("profile", subagent.NodeRunning)
	event.Snapshot.Nodes = append(event.Snapshot.Nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: "other-root", SessionID: "other"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "unrelated", State: subagent.NodeRunning}}}})
	root.Update(event)
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "1/1 subagent")
	require.True(t, root.panelAnimation.IsActive())
	event.Snapshot.Nodes[0].Children[0].Node.State = subagent.NodeStopped
	require.Equal(t, subagent.NodeRunning, root.panelData["profile"].nodes[0].Node.State, "event data copied")
	items := []session.Todo{{ID: "one", Description: "First", Status: "completed"}, {ID: "two", Description: "Second", Status: "pending"}}
	root.Update(todoEvent(items))
	items[0].Status = "pending"
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "1/2 todos")
	for _, state := range []subagent.NodeState{subagent.NodeStarting, subagent.NodeIdle, subagent.NodeStopped} {
		root.Update(panelTree("profile", state))
		require.False(t, root.panelAnimation.IsActive())
		require.NotContains(t, ansi.Strip(root.renderMessageBar()), "active")
	}
	event = panelTree("profile", subagent.NodeRunning)
	event.Snapshot.Nodes[0].Children[0].Node.WaitingOn = "approval"
	root.Update(event)
	require.False(t, root.panelAnimation.IsActive(), "waiting is not running")
	root.Update(panelTree("profile", subagent.NodeRunning))
	root.Update(tea.BlurMsg{})
	require.False(t, root.panelAnimation.IsActive())
	root.Update(tea.FocusMsg{})
	require.True(t, root.panelAnimation.IsActive())
	root.Update(dialog.OpenDialogMsg{Model: dialog.NewPanelDetailsDialog("test", nil)})
	require.False(t, root.panelAnimation.IsActive(), "modal obscures panel")
	root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{}})
	require.False(t, root.panelAnimation.IsActive())
}

func TestPanelClickZonesAndModalIsolation(t *testing.T) {
	root := panelFixture(t)
	for _, zone := range root.panelFrame().Zones {
		for _, x := range []int{zone.Start, zone.End - 1} {
			_, cmd := root.handleMouseClick(tea.MouseClickMsg{X: x + messageBarOrigin(root.width), Y: 39, Button: tea.MouseLeft})
			opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
			require.True(t, ok)
			require.NotNil(t, opened.Model)
		}
	}
	root.Update(dialog.OpenDialogMsg{Model: dialog.NewPanelDetailsDialog("modal", []string{"stay"})})
	zone := root.panelFrame().Zones[0]
	_, cmd := root.handleMouseClick(tea.MouseClickMsg{X: zone.Start + 1, Y: 39, Button: tea.MouseLeft})
	_, opened := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.False(t, opened)
}

func TestPanelTodoHydrationRejectsStaleErrorAndReset(t *testing.T) {
	root := panelFixture(t)
	data := root.panelData["profile"]
	data.requested = true
	msg := panelTodosMsg{owner: "profile", sessionID: data.sessionID, generation: data.generation, revision: data.revision, selection: root.panelSelectionGeneration, todos: []session.Todo{{Status: "completed"}}}
	root.acceptPanelTodos(msg)
	require.True(t, data.todosKnown)
	data.todosKnown = false
	data.requested = true
	failed := msg
	failed.err = errors.New("unavailable")
	root.acceptPanelTodos(failed)
	require.False(t, data.todosKnown)
	data.requested = true
	root.ingestPanelEvent("profile", todoEvent([]session.Todo{{Status: "pending"}}))
	root.acceptPanelTodos(msg)
	require.Equal(t, "pending", data.todos[0].Status)
	root.ingestPanelEvent("profile", &app.SessionResetEvent{})
	current := root.panelData["profile"]
	current.requested = true
	root.acceptPanelTodos(msg)
	require.False(t, current.todosKnown, "old response cannot survive reset of same canonical session")
	currentMsg := msg
	currentMsg.revision = current.revision
	currentMsg.selection = root.panelSelectionGeneration + 1
	root.acceptPanelTodos(currentMsg)
	require.False(t, current.todosKnown, "focus revision fences delayed hydration")
}

type panelTodoHandle struct {
	*lifecycleHandle
	capable bool
	calls   int
	items   []session.Todo
	failure error
}

func (h *panelTodoHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.ID(), AgentName: "root", Capabilities: runtime.SessionCapabilities{Todos: h.capable, TodoEditing: h.capable}}
}
func (h *panelTodoHandle) Todos(context.Context) ([]session.Todo, error) {
	h.calls++
	return h.items, h.failure
}

type panelTodoRuntime struct {
	runtime.SessionRuntime
	handle *panelTodoHandle
}

func (r *panelTodoRuntime) SessionByID(string) (runtime.SessionHandle, error) { return r.handle, nil }

func TestPanelTodoHydrationCapabilityOneShotAndEventWins(t *testing.T) {
	for _, capable := range []bool{false, true} {
		root, _, _ := frozenClockRoot(t, 120, 40)
		sess := root.application.Session()
		handle := &panelTodoHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, capable: capable, items: []session.Todo{{ID: "stored", Status: "completed"}}}
		application := app.New(t.Context(), &panelTodoRuntime{handle: handle}, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
		require.Same(t, handle, application.SessionHandle())
		root.application = application
		root.supervisor.GetRunner("profile").App = application
		_, cmd := root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelTodos}})
		require.Zero(t, handle.calls, "owner loop does not call Todos")
		require.Nil(t, root.preparePanel(), "one in-flight hydration")
		if !capable {
			require.Nil(t, cmd)
			require.Contains(t, ansi.Strip(root.renderMessageBar()), "unavailable")
			continue
		}
		require.NotNil(t, cmd)
		msg, ok := cmd().(panelTodosMsg)
		require.True(t, ok)
		require.Equal(t, 1, handle.calls)
		handle.items[0].Status = "pending"
		root.acceptPanelTodos(msg)
		require.Contains(t, ansi.Strip(root.renderMessageBar()), "1/1 todos", "hydration payload detached")
		require.Nil(t, root.preparePanel(), "no polling after hydration")
		root.panelData["profile"].attempted = false
		cmd = root.preparePanel()
		require.NotNil(t, cmd)
		handle.items = []session.Todo{{ID: "fresh", Status: "pending"}}
		root.ingestPanelEvent("profile", todoEvent([]session.Todo{{ID: "stale", Status: "pending"}}))
		root.acceptPanelTodos(cmd().(panelTodosMsg))
		fresh := root.preparePanel()
		require.NotNil(t, fresh)
		root.acceptPanelTodos(fresh().(panelTodosMsg))
		require.Equal(t, "fresh", root.panelData["profile"].todos[0].ID)
	}
}

func TestPanelBackgroundRoutesAndSessionSelectionStayIsolated(t *testing.T) {
	root := splitTestRoot(t)
	root.applyPanelSettings(messages.DefaultPanelSettings())
	t.Cleanup(root.panelAnimation.Stop)
	generation, _ := root.supervisor.RouteGeneration("second")
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation, Inner: todoEvent([]session.Todo{{ID: "second-todo", Status: "completed"}})})
	require.False(t, root.panelData["profile"].todosKnown)
	require.True(t, root.panelData["second"].todosKnown)
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation + 1, Inner: todoEvent(nil)})
	require.Len(t, root.panelData["second"].todos, 1, "stale route rejected before cache ingestion")
	root.Update(messages.SwitchTabMsg{SessionID: "second"})
	require.Contains(t, ansi.Strip(root.renderMessageBar()), "1/1 todos")
	root.Update(messages.SwitchTabMsg{SessionID: "profile"})
	require.NotContains(t, ansi.Strip(root.renderMessageBar()), "1/1 todos")
}

func TestPanelSpinnerAdvancesOnOwnerClockAndStopsIdle(t *testing.T) {
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(t, 120, 40, animation.NewRuntimeWithScheduler(scheduler))
	root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelSubagents}})
	t.Cleanup(root.panelAnimation.Stop)
	root.ingestPanelEvent("profile", panelTree("profile", subagent.NodeRunning))
	root.preparePanel()
	before := root.View().Content
	zones := root.panelFrame().Zones
	frames := animation.TabBusy.Frames()
	require.Equal(t, animation.Chat.Frames(), frames, "same Braille frames as the sidebar")
	seen := make(map[string]bool)
	changed := false
	cycle := time.Duration(len(frames)) * animation.TabBusy.DefaultFrameDuration()
	for root.ar.Now() < cycle {
		frame := animation.TabBusy.FrameAt(root.ar.Now())
		require.Equal(t, frame+" 1/1 subagent", root.panelElements()[0].Label)
		require.Contains(t, root.View().Content, frame)
		changed = changed || before != root.View().Content
		require.Equal(t, 1, ansi.StringWidth(frame))
		runes := []rune(frame)
		require.Len(t, runes, 1)
		require.GreaterOrEqual(t, runes[0], rune(0x2800))
		require.LessOrEqual(t, runes[0], rune(0x28ff))
		seen[frame] = true
		panel := root.panelFrame()
		require.Equal(t, zones, panel.Zones, "spinner phase does not move hit targets")
		require.Equal(t, messageBarWidth(root.width), ansi.StringWidth(panel.Content))
		for _, zone := range zones {
			for x := zone.Start; x < zone.End; x++ {
				id, hit := panel.Hit(x)
				require.True(t, hit)
				require.Equal(t, zone.ID, id)
			}
		}
		cmd := root.ar.Continue()
		require.NotNil(t, cmd)
		root.updateWithLifecycle(cmd())
	}
	require.Len(t, seen, len(frames), "all canonical phases were rendered")
	require.True(t, changed, "panel participates in root cached frame dirty boundaries")
	root.Update(panelTree("profile", subagent.NodeIdle))
	require.Equal(t, "1 subagent", root.panelElements()[0].Label)
	require.False(t, root.panelAnimation.IsActive())
	require.Zero(t, root.ar.ActiveCount())
	require.Nil(t, root.ar.Continue(), "idle panel schedules no perpetual ticks")
	require.Equal(t, root.View().Content, root.View().Content)
}
