package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
)

func TestDialogCursorInputFollowsFocus(t *testing.T) {
	t.Parallel()

	t.Run("plan name", func(t *testing.T) {
		t.Parallel()
		d := newPlanNameDialog().(*planNameDialog)
		require.NotNil(t, d.CursorInput())
		assert.Same(t, cursor.Input(&d.input), d.CursorInput())
		d.input.Blur()
		assert.Nil(t, d.CursorInput())
	})

	t.Run("plan status", func(t *testing.T) {
		t.Parallel()
		d := newPlanStatusDialog("plan", "todo", 1).(*planStatusDialog)
		require.NotNil(t, d.CursorInput())
		d.input.Blur()
		assert.Nil(t, d.CursorInput())
	})

	t.Run("plan browser filter", func(t *testing.T) {
		t.Parallel()
		d := NewPlanBrowserDialog(testPlanListing()).(*planBrowserDialog)
		assert.Equal(t, d.filterInput.Focused(), d.CursorInput() != nil)
		d.filterInput.Focus()
		assert.NotNil(t, d.CursorInput())
		d.filterInput.Blur()
		assert.Nil(t, d.CursorInput())
	})

	t.Run("multi choice custom answer", func(t *testing.T) {
		t.Parallel()
		d := NewMultiChoiceDialog(MultiChoiceConfig{
			DialogID: "cursor-test",
			Title:    "Pick",
			Options:  []MultiChoiceOption{{ID: "a", Label: "A", Value: "a"}},
		}).(*multiChoiceDialog)
		d.customInput.Blur()
		assert.Nil(t, d.CursorInput())
		d.customInput.Focus()
		assert.Same(t, cursor.Input(&d.customInput), d.CursorInput())
	})

	t.Run("todo and pending editors", func(t *testing.T) {
		t.Parallel()
		todo := NewTodoEditDialog(messages.TodoScope{}, "id", "text", 1, nil).(*todoEditDialog)
		assert.NotNil(t, todo.CursorInput())
		todo.input.Blur()
		assert.Nil(t, todo.CursorInput())

		pending := NewPendingMessageEditDialog("s", "t", "text", 1, nil).(*pendingMessageEditDialog)
		assert.NotNil(t, pending.CursorInput())
		pending.input.Blur()
		assert.Nil(t, pending.CursorInput())
	})
}

func TestElicitationCursorInputReturnsOnlyFocusedField(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"first":  map[string]any{"type": "string"},
			"second": map[string]any{"type": "string"},
		},
		"required": []any{"first", "second"},
	}
	d := NewElicitationDialog("two fields", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	require.Len(t, d.inputs, 2)

	for i := range d.inputs {
		d.inputs[i].Blur()
	}
	assert.Nil(t, d.CursorInput(), "no focused field means no cursor")

	d.focusField(1)
	assert.Same(t, cursor.Input(&d.inputs[1]), d.CursorInput())

	d.focusField(0)
	assert.Same(t, cursor.Input(&d.inputs[0]), d.CursorInput())
}

func TestDialogInitDoesNotStartBlink(t *testing.T) {
	t.Parallel()

	assert.Nil(t, newPlanNameDialog().Init())
	assert.Nil(t, NewMultiChoiceDialog(MultiChoiceConfig{DialogID: "x", Title: "x", Options: []MultiChoiceOption{{ID: "a", Label: "A", Value: "a"}}}).Init())
}

func TestManagerInvalidateCursorViewMarksEntriesDirty(t *testing.T) {
	t.Parallel()

	m := New(animation.NewRuntime()).(*manager)
	m.visualDirty = false
	m.InvalidateCursorView() // empty stack must not panic
	assert.True(t, m.visualDirty)

	entry := dialogEntry{animatedDialog: &animatedDialog{viewValid: true}}
	m.stack = append(m.stack, entry)
	m.visualDirty = false
	m.InvalidateCursorView()
	assert.False(t, m.stack[0].viewValid)
	assert.True(t, m.visualDirty)
}
