package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestTodosEditRemoveRefreshStableSelection(t *testing.T) {
	scope := messages.TodoScope{Owner: "route", SessionID: "session", Generation: 3}
	items := []session.Todo{{ID: "one", Description: "First 世界", Status: "pending"}, {ID: "two", Description: "Second 👩‍💻", Status: "in-progress"}, {ID: "three", Description: "Third", Status: "completed"}}
	d := NewTodosDialog(scope, items, "two").(*todosDialog)
	d.SetSize(100, 24)
	view := ansi.Strip(d.View())
	for _, status := range []string{"○ × ✎", "◐ × ✎", "● × ✎"} {
		require.Contains(t, view, status)
	}
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "two", Status: "completed"}, cmd())
	items[1].Status = "completed"
	d.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: items})
	require.Equal(t, 1, d.selected)
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
	require.Contains(t, ansi.Strip(d.View()), "again to remove")
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "two", Remove: true}, cmd())
	d.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{items[0], items[2]}})
	require.Equal(t, "three", d.todos[d.selected].ID)
	for _, size := range [][2]int{{30, 12}, {15, 8}, {6, 4}} {
		d.SetSize(size[0], size[1])
		require.LessOrEqual(t, lipgloss.Width(d.View()), size[0])
		require.LessOrEqual(t, lipgloss.Height(d.View()), size[1])
	}
}

func TestTodosMouseStatusAndRemoveTargets(t *testing.T) {
	scope := messages.TodoScope{SessionID: "session"}
	items := []session.Todo{{ID: "opaque", Description: "界é", Status: "pending"}}
	d := NewTodosDialog(scope, items, "").(*todosDialog)
	d.SetSize(80, 20)
	x, y, _, _ := d.BodyScrollBounds()
	_, cmd := d.Update(tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "opaque", Status: "in-progress"}, cmd())
	d.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: items})
	_, cmd = d.Update(tea.MouseClickMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	require.Nil(t, cmd)
	_, cmd = d.Update(tea.MouseClickMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "opaque", Remove: true}, cmd())
}

func TestLongUnicodeTodoRemainsScrollable(t *testing.T) {
	d := NewTodosDialog(messages.TodoScope{}, []session.Todo{{ID: "long", Status: "pending", Description: strings.Repeat("界é long description ", 60) + "END-OF-TODO"}}, "").(*todosDialog)
	for _, width := range []int{80, 30, 15} {
		d.SetSize(width, 14)
		for range len(d.lines.Lines()) {
			d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
		}
		require.Positive(t, d.scrollview.ScrollOffset())
		// All words remain in the scrollable model even when footer and columns shrink.
		var plain []string
		for _, line := range d.lines.Lines() {
			plain = append(plain, strings.TrimSpace(ansi.Strip(line)))
		}
		require.Contains(t, strings.Join(plain, ""), "END-OF-TODO")
		require.Contains(t, ansi.Strip(d.View()), "O")
	}
}

func TestTodosWordWrapAlignedControlsAndSpacers(t *testing.T) {
	items := []session.Todo{
		{ID: "one", Status: "pending", Description: "\x1b[31mBright\x1b[0m animations preserve Unicode 👩‍💻 界é words across sensible boundaries " + strings.Repeat("x", 90) + " FINISH"},
		{ID: "two", Status: "in-progress", Description: "Second task"},
		{ID: "three", Status: "completed", Description: "Third task"},
	}
	d := NewTodosDialog(messages.TodoScope{}, items, "one").(*todosDialog)
	d.SetSize(80, 35)
	plain := ansi.Strip(strings.Join(d.lines.Lines(), "\n"))
	for _, word := range []string{"Bright", "animations", "preserve", "Unicode", "👩‍💻", "界é", "boundaries", "FINISH"} {
		require.Contains(t, plain, word, "ordinary words and graphemes must not break")
	}
	for i, g := range d.prepared {
		require.Equal(t, 4, g.remove)
		require.Equal(t, 2, g.statusStart)
		line := ansi.Strip(d.lines.Lines()[g.start])
		if i > 0 {
			require.Equal(t, "", d.lines.Lines()[g.start-1])
			x, y := d.bodyX+4, d.bodyY+g.start-1
			index, _, _ := d.hitAt(x, y)
			require.Equal(t, -1, index)
			_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Nil(t, cmd)
			require.Equal(t, 0, d.selected)
		}
		require.Equal(t, '×', []rune(line)[4])
	}
	// Continuation lines select the owning stable ID without activating controls.
	_, cmd := d.Update(tea.MouseClickMsg{X: d.bodyX + 4, Y: d.bodyY + 1, Button: tea.MouseLeft})
	require.Nil(t, cmd)
	require.Equal(t, "one", d.todos[d.selected].ID)
}

func TestTodosHoverFadeSelectionAndIdle(t *testing.T) {
	r := newDialogRuntime()
	d := NewTodosDialog(messages.TodoScope{}, []session.Todo{
		{ID: "one", Status: "pending", Description: "First task"},
		{ID: "two", Status: "completed", Description: "Second task"},
	}, "one").(*todosDialog)
	d.BindAnimationRuntime(r)
	t.Cleanup(d.Cleanup)
	d.SetSize(80, 25)
	require.Nil(t, r.Continue())
	baseline := d.View()
	x, y := d.bodyX+20, d.bodyY+d.prepared[1].start
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, "two", d.hovered)
	require.Equal(t, 0, d.selected, "hover does not select")
	require.Equal(t, int32(1), r.ActiveCount())
	tick := acceptedDialogTick(r, r.Continue())
	d.Update(tick)
	require.True(t, tick.Dirty())
	require.Greater(t, d.hover["two"].value, 0.0)
	require.Less(t, d.hover["two"].value, 1.0)
	require.NotEqual(t, baseline, d.View())
	require.Equal(t, ansi.Strip(baseline), ansi.Strip(d.View()), "hover preserves selection marker and geometry")
	for r.HasActive() {
		d.Update(acceptedDialogTick(r, r.Continue()))
	}
	require.Equal(t, 1.0, d.hover["two"].value)
	require.Nil(t, r.Continue(), "stationary hover does not tick")
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, 1, d.selected)
	require.Equal(t, "two", d.hovered)
	d.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	d.Update(acceptedDialogTick(r, r.Continue()))
	require.Greater(t, d.hover["two"].value, 0.0)
	require.Less(t, d.hover["two"].value, 1.0)
	for r.HasActive() {
		d.Update(acceptedDialogTick(r, r.Continue()))
	}
	require.Empty(t, d.hover)
	require.Nil(t, r.Continue())
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, r.HasActive())
	d.StopAnimations()
	require.False(t, r.HasActive())
	require.False(t, d.pointerKnown)
	require.Empty(t, d.hover)
	require.Equal(t, 1, d.selected)
}

func TestTodosPointerRehitAfterWheelResizeAndSnapshot(t *testing.T) {
	items := make([]session.Todo, 15)
	for i := range items {
		items[i] = session.Todo{ID: fmt.Sprint(i), Status: "pending", Description: "A readable task description with several words"}
	}
	d := NewTodosDialog(messages.TodoScope{}, items, "0").(*todosDialog)
	r := newDialogRuntime()
	d.BindAnimationRuntime(r)
	t.Cleanup(d.Cleanup)
	d.SetSize(100, 20)
	x, y := d.bodyX+10, d.bodyY
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, "0", d.hovered)
	d.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown})
	require.Equal(t, "1", d.hovered)
	require.Equal(t, 0, d.selected)
	d.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: 1})
	require.Equal(t, "2", d.hovered)
	d.SetSize(35, 20)
	i, _, _ := d.hitAt(x, y)
	want := ""
	if i >= 0 {
		want = d.todos[i].ID
	}
	require.Equal(t, want, d.hovered, "resize re-hits the last pointer, not its old row")
	d.Update(messages.TodosSnapshotMsg{Todos: []session.Todo{{ID: "new", Status: "completed", Description: "Replacement"}}})
	for id := range d.hover {
		require.Equal(t, "new", id, "stale IDs are pruned")
	}
	d.Update(messages.TodosSnapshotMsg{})
	require.Empty(t, d.hover)
	require.Empty(t, d.hovered)
	require.False(t, r.HasActive())
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
}

func TestTodosNarrowGeometryAndMutationGuards(t *testing.T) {
	scope := messages.TodoScope{Owner: "owner", SessionID: "s", Generation: 7}
	items := []session.Todo{{ID: "one", Status: "pending", Description: "Full Unicode 世界 👩‍💻 description END"}, {ID: "two", Status: "completed", Description: "Other task"}}
	d := NewTodosDialog(scope, items, "one").(*todosDialog)
	for _, width := range []int{40, 30, 20, 15, 6} {
		d.SetSize(width, 18)
		for _, g := range d.prepared {
			require.Less(t, g.remove, g.width)
			require.LessOrEqual(t, g.statusEnd, g.width)
		}
		for _, line := range d.lines.Lines() {
			require.LessOrEqual(t, ansi.StringWidth(line), d.prepared[0].width)
		}
		require.LessOrEqual(t, lipgloss.Width(d.View()), width)
	}
	d.SetSize(80, 24)
	_, cmd := d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
	require.False(t, d.DialogClosable())
	d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, d.removeArmed)
	require.True(t, d.DialogClosable())
	_, cmd = d.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "one", Status: "in-progress"}, cmd())
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd, "busy dialogs do not arm a removal")
	require.Empty(t, d.removeArmed)
	d.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "wrong"}, Todos: nil})
	require.True(t, d.busy)
	require.Len(t, d.todos, 2)
	d.Update(messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{items[1], items[0]}})
	require.Equal(t, "one", d.todos[d.selected].ID)
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Empty(t, d.removeArmed, "moving selection disarms removal")
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Nil(t, cmd)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyDelete})
	require.Equal(t, messages.EditTodoMsg{Scope: scope, ID: "two", Remove: true}, cmd())
}

func TestTodosManagerEscapeCancelsRemovalAndOcclusionStopsHover(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 100, height: 30}
	t.Cleanup(mgr.Cleanup)
	d := NewTodosDialog(messages.TodoScope{}, []session.Todo{{ID: "one", Status: "pending", Description: "Task"}}, "one").(*todosDialog)
	mgr.Update(OpenDialogMsg{Model: d})
	for r.HasActive() {
		mgr.Update(acceptedDialogTick(r, r.Continue()))
	}
	mgr.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	require.Equal(t, "one", d.removeArmed)
	_, cmd := mgr.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Nil(t, cmd)
	require.Empty(t, d.removeArmed)
	require.False(t, mgr.Closing())
	for r.HasActive() {
		mgr.Update(acceptedDialogTick(r, r.Continue()))
	}
	mgr.Update(tea.MouseMotionMsg{X: d.bodyX + 5, Y: d.bodyY})
	require.True(t, d.hoverAnimation.IsActive())
	mgr.Update(OpenDialogMsg{Model: NewTodosDialog(messages.TodoScope{}, nil, "")})
	require.False(t, d.hoverAnimation.IsActive())
	require.False(t, d.pointerKnown)
	for r.HasActive() {
		mgr.Update(acceptedDialogTick(r, r.Continue()))
	}
	require.Nil(t, r.Continue())
}

func TestTodosKeyboardStatusCycleAndSnapshotError(t *testing.T) {
	d := NewTodosDialog(messages.TodoScope{}, []session.Todo{{ID: "one", Status: "pending", Description: "Task"}}, "one").(*todosDialog)
	d.SetSize(80, 20)
	for _, tc := range []struct {
		key    tea.KeyPressMsg
		status string
	}{
		{tea.KeyPressMsg{Code: ' ', Text: " "}, "in-progress"},
		{tea.KeyPressMsg{Code: ' ', Text: " "}, "completed"},
		{tea.KeyPressMsg{Code: ' ', Text: " "}, "pending"},
		{tea.KeyPressMsg{Code: '3', Text: "3"}, "completed"},
		{tea.KeyPressMsg{Code: '1', Text: "1"}, "pending"},
	} {
		_, cmd := d.Update(tc.key)
		require.NotNil(t, cmd)
		require.Equal(t, messages.EditTodoMsg{ID: "one", Status: tc.status}, cmd())
		d.Update(messages.TodosSnapshotMsg{Todos: []session.Todo{{ID: "one", Status: tc.status, Description: "Task"}}})
	}
	d.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	d.Update(messages.TodosSnapshotMsg{Err: fmt.Errorf("save failed")})
	require.False(t, d.busy)
	require.Equal(t, "pending", d.todos[0].Status)
	require.Contains(t, ansi.Strip(d.View()), "save failed")
}
