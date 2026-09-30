package dialog

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestTodoEditorDraftCorrelationAndExplicitConflictRetry(t *testing.T) {
	scope := messages.TodoScope{Owner: "owner", SessionID: "session", Generation: 7, Epoch: 2}
	calls := 0
	d := NewTodoEditDialog(scope, "todo", "original", 17, func(requestID uint64, expected, description string) tea.Cmd {
		calls++
		if calls == 1 {
			require.Equal(t, "original", expected)
		} else {
			require.Equal(t, "concurrent", expected)
		}
		require.Equal(t, "  draft 世界é\nsecond line  ", description)
		return func() tea.Msg {
			return messages.TodoSaveResultMsg{Scope: scope, ID: "todo", EditorID: 17, RequestID: requestID}
		}
	}).(*todoEditDialog)
	d.SetSize(80, 24)
	d.input.SetValue("  draft 世界é\nsecond line  ")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	require.True(t, d.saving)
	snapshot := messages.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "todo", Description: "concurrent", Status: "completed"}}}
	d.Update(snapshot)
	require.True(t, d.saving, "snapshots cannot finish a save")
	require.Equal(t, "  draft 世界é\nsecond line  ", d.input.Value())
	d.Update(messages.TodoSaveResultMsg{Scope: scope, ID: "todo", EditorID: 16})
	wrong := scope
	wrong.Epoch++
	d.Update(messages.TodoSaveResultMsg{Scope: wrong, ID: "todo", EditorID: 17})
	require.True(t, d.saving)
	d.Update(messages.TodoSaveResultMsg{Scope: scope, ID: "todo", EditorID: 17, RequestID: 1, Err: errors.New("description conflict"), Todos: snapshot.Todos})
	require.False(t, d.saving)
	require.False(t, d.closed)
	require.Equal(t, "original", d.expected, "conflict must not silently change CAS precondition")
	require.Contains(t, ansi.Strip(d.View()), "description conflict")
	d.Update(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	require.Equal(t, "concurrent", d.expected)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	d.Update(messages.TodoSaveResultMsg{Scope: scope, ID: "todo", EditorID: 17, RequestID: 1})
	require.True(t, d.saving, "duplicate previous result cannot finish retry")
	_, closeCmd := d.Update(cmd())
	require.NotNil(t, closeCmd)
	require.IsType(t, CloseDialogByModelMsg{}, closeCmd())
	require.Equal(t, 2, calls)
}

func TestTodoEditorBlankCancelAndControls(t *testing.T) {
	scope := messages.TodoScope{SessionID: "s"}
	calls := 0
	d := NewTodoEditDialog(scope, "todo", "original", 1, func(uint64, string, string) tea.Cmd { calls++; return func() tea.Msg { return nil } }).(*todoEditDialog)
	d.SetSize(80, 24)
	d.input.SetValue(" \n\t\u2003")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	require.Nil(t, cmd)
	require.Zero(t, calls)
	require.False(t, d.saving)
	require.Contains(t, ansi.Strip(d.View()), "cannot be blank")
	require.Contains(t, ansi.Strip(d.View()), "Save")
	require.Contains(t, ansi.Strip(d.View()), "Cancel")
	require.NotContains(t, strings.ToLower(ansi.Strip(d.View())), " esc")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	require.Zero(t, calls)
	d.Update(messages.TodoSaveResultMsg{Scope: scope, ID: "todo", EditorID: 1, Err: errors.New("late")})
	require.NotEqual(t, "late", d.saveError)
}

func TestTodoListOpensSharedEditorByKeyboardAndGlyph(t *testing.T) {
	scope := messages.TodoScope{SessionID: "s"}
	d := NewTodosDialog(scope, []session.Todo{{ID: "one", Description: "first", Status: "pending"}, {ID: "two", Description: "second", Status: "completed"}}, "two").(*todosDialog)
	d.SetSize(80, 24)
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'e', Text: "e"}} {
		_, cmd := d.Update(key)
		require.NotNil(t, cmd)
		require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "two"}, cmd())
	}
	g := d.prepared[0]
	_, cmd := d.Update(tea.MouseClickMsg{X: d.bodyX + g.edit, Y: d.bodyY + g.start, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	require.Equal(t, messages.OpenTodoEditMsg{Scope: scope, ID: "one"}, cmd())
}

func TestTodoDialogHoverRetainsPreparedRows(t *testing.T) {
	d := NewTodosDialog(messages.TodoScope{}, []session.Todo{{ID: "one", Description: strings.Repeat("long words ", 50), Status: "completed"}}, "one").(*todosDialog)
	d.SetSize(60, 20)
	original := &d.rowCache["one"].rows[0]
	lines := d.lines
	d.Update(tea.MouseMotionMsg{X: d.bodyX + 12, Y: d.bodyY})
	for range 20 {
		d.View()
	}
	require.Same(t, original, &d.rowCache["one"].rows[0], "hover and View never rewrap")
	require.Same(t, lines, d.lines, "hover does not rebuild offscreen geometry")
}
