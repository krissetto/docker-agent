package textinput

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestSingleLineSanitizationAndCursor(t *testing.T) {
	m := New()
	m.SetValue("a\tb\nc\r\nd")
	require.Equal(t, "a b c d", m.Value())
	require.Equal(t, 7, m.Position())
	m.SetCursor(2)
	m.SetValue("longer")
	require.Equal(t, 2, m.Position())
	m.SetValue("x")
	require.Equal(t, 1, m.Position())
	m.InsertString("\t\r\n")
	require.Equal(t, "x  ", m.Value())
	m.CharLimit = 2
	m.SetValue("éX")
	require.Equal(t, "é", m.Value())
	m.Focus()
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	require.Empty(t, m.Value())
}
func TestCopyIsolationReadOnlyAndWideHitTesting(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.SetWidth(10)
	m.SetValue(strings.Repeat("界語", 20))
	m.Focus()
	m.SetCursor(m.Position() - 2)
	layout, revision, view := m.Layout(), m.Revision(), m.View()
	offset := m.PositionAtCell(0)
	require.Positive(t, offset)
	require.Equal(t, offset, m.PositionAtCell(1))
	require.Equal(t, offset+1, m.PositionAtCell(2))
	copy := m
	copy.SetCursor(offset)
	copy.InsertString("X")
	copy.SetWidth(3)
	for range 3 {
		require.Equal(t, view, m.View())
		require.Equal(t, layout, m.Layout())
		require.Equal(t, revision, m.Revision())
	}
}
func TestPasswordAndHiddenEcho(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.SetWidth(4)
	m.EchoMode = EchoPassword
	m.SetValue("界é👩‍💻")
	m.Focus()
	require.Equal(t, "***  ", ansi.Strip(m.View()))
	require.Equal(t, 5, ansi.StringWidth(m.View()))
	for _, row := range m.Layout().Rows {
		require.NotContains(t, row.Text, "界")
		for _, run := range row.Runs {
			require.NotContains(t, run.Text, "e")
		}
	}
	m.EchoMode = EchoNone
	m.SetWidth(4)
	require.Equal(t, "     ", ansi.Strip(m.View()))
	require.Equal(t, "界é👩‍💻", m.Value())
}
func TestOpaqueStylesAndWidth(t *testing.T) {
	m := New()
	m.Prompt = ""
	m.SetWidth(6)
	m.SetValue("abc")
	s := m.Styles()
	s.Blurred.Text = lipgloss.NewStyle().Transform(func(v string) string { return strings.ReplaceAll(v, "abc", "XYZ") })
	m.SetStyles(s)
	require.Equal(t, "XYZ    ", ansi.Strip(m.View()))
	require.Equal(t, 7, ansi.StringWidth(m.View()))
	s.Blurred.Text = s.Blurred.Text.Bold(true)
	require.False(t, m.Styles().Blurred.Text.GetBold())
	m.Focus()
	m.SetVirtualCursor(false)
	c := m.Cursor()
	c.X = 999
	require.NotEqual(t, c.X, m.Cursor().X)
}
