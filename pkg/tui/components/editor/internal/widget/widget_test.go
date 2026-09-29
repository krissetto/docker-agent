package widget

import (
	"testing"
	"time"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

func TestTextareaOwnsMutationAndVirtualCursor(t *testing.T) {
	w := NewTextarea()
	w.SetWidth(24)
	w.SetHeight(2)
	w.SetValue("界 cafe\u0301")
	w.Focus()
	require.Nil(t, w.Cursor(), "default private virtual cursor must not be guessed from public cursor")
	style := w.Styles()
	style.Cursor.BlinkSpeed = time.Millisecond
	w.SetStyles(style)
	_, blink := w.Update(cursor.Blink())
	require.NotNil(t, blink)
	visible := w.View()
	renders := w.Renders()
	require.Equal(t, visible, w.View())
	require.Equal(t, renders, w.Renders())
	// Deliver the genuine widget-owned blink ID, not a guessed public BlinkMsg.
	_, next := w.Update(blink())
	require.NotNil(t, next)
	require.Equal(t, renders, w.Renders(), "blink update does not eagerly render")
	hidden := w.View()
	require.NotEqual(t, visible, hidden)
	require.Equal(t, hidden, w.FreshView())

	binding := w.NewlineBinding()
	binding.Keys()[0] = "tampered"
	require.NotEqual(t, binding.Keys(), w.NewlineBinding().Keys(), "binding slice is detached")
	original := w.Styles().Focused.Text.GetBold()
	style = w.Styles()
	style.Focused.Text = style.Focused.Text.Bold(!original)
	require.Equal(t, original, w.Styles().Focused.Text.GetBold(), "Lipgloss value setters cannot mutate widget styles")
}

func TestTextareaSameValueStillResetsSelection(t *testing.T) {
	w := NewTextarea()
	w.Focus()
	w.SetValue("abc")
	w.MoveToEnd()
	w.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	require.NotEmpty(t, w.SelectedText())
	before := w.Generation()
	w.SetValue(w.Value())
	require.Greater(t, w.Generation(), before)
	require.Empty(t, w.SelectedText())
	require.Equal(t, w.View(), w.FreshView())
}

func TestTextinputMutationArtifacts(t *testing.T) {
	w := NewTextinput()
	w.SetWidth(20)
	w.Focus()
	w.SetValue("query")
	before := w.View()
	renders := w.Renders()
	require.Equal(t, before, w.View())
	require.Equal(t, renders, w.Renders())
	w.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	require.Equal(t, renders, w.Renders())
	require.Equal(t, w.View(), w.FreshView())
	s := w.Styles()
	s.Focused.Text = lipgloss.NewStyle().Bold(true)
	require.False(t, w.Styles().Focused.Text.GetBold())
	w.SetStyles(s)
	require.Equal(t, w.View(), w.FreshView())
}
