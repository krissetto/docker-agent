package messagebar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestPersistentControlsDoNotFadeOrExpireAndActionsWin(t *testing.T) {
	m := New()
	m.SetSize(100, 1)
	m.SetControls([]Control{{ID: "one", Label: "1 Build 世界", Active: true, Command: func() tea.Msg { return "workspace" }}, {ID: "two", Label: "2 Review", Command: func() tea.Msg { return "other" }}})
	m.SetMessage(Message{Text: "Warning notice", Actions: []Action{{Label: "Confirm", Command: func() tea.Msg { return "confirm" }}}})
	require.Contains(t, ansi.Strip(m.View()), "Build 世界")
	require.Contains(t, ansi.Strip(m.View()), "Warning notice")
	require.Equal(t, 100, ansi.StringWidth(m.View()))
	require.Equal(t, "workspace", m.ControlClick(2, 0)())
	for _, b := range m.bounds {
		require.GreaterOrEqual(t, b.start, m.controlWidth)
	}
	m.ClearMessage()
	require.Contains(t, ansi.Strip(m.View()), "Build 世界")
	require.NotContains(t, ansi.Strip(m.View()), "Warning notice")
	m.SetMessage(Message{Text: "urgent", Actions: []Action{{Label: "Confirm", Command: func() tea.Msg { return "confirm" }}}})
	m.SetSize(12, 1)
	require.Contains(t, ansi.Strip(m.View()), "Confirm")
	require.Empty(t, m.controlBounds)
	require.Equal(t, 12, ansi.StringWidth(m.View()))
	m.SetSize(100, 1)
	m.ClearMessage()
	m.SetFallback("paused /resume")
	require.Contains(t, ansi.Strip(m.View()), "paused /resume")
	require.NotEmpty(t, strings.TrimSpace(ansi.Strip(m.View())))
}

func TestFallbackUsesTheSameRenderedControlHitboxes(t *testing.T) {
	m := New()
	m.SetSize(38, 1)
	m.SetControls([]Control{{ID: "one", Label: "1 Alpha 世界", Active: true, Command: func() tea.Msg { return "one" }}, {ID: "two", Label: "2 Beta", Command: func() tea.Msg { return "two" }}})
	m.SetFallback("Restored · paused · /resume with a long fallback")
	view := ansi.Strip(m.View())
	for _, b := range m.controlBounds {
		require.Contains(t, ansi.Cut(view, b.start, b.end), b.label)
		require.Equal(t, m.controls[b.index].ID, m.ControlClick(b.start, 0)())
	}
	m.SetFallback("")
	require.Equal(t, 38, ansi.StringWidth(m.View()))
}
