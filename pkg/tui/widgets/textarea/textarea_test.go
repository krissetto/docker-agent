package textarea

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/text"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
	"github.com/stretchr/testify/require"
)

func plainArea(width, height int) Model {
	m := New()
	m.Prompt = ""
	m.ShowLineNumbers = false
	m.SetWidth(width)
	m.SetHeight(height)
	return m
}
func TestValueCopiesAndReadOnlyRendering(t *testing.T) {
	m := plainArea(9, 2)
	m.SetValue("one\ntwo\n界é👩‍💻tail")
	m.Focus()
	snapshot, revision, value := m.Layout(), m.Revision(), m.Value()
	copy := m
	copy.MoveToBegin()
	copy.InsertString("new")
	copy.SetWidth(4)
	copy.SetHeight(1)
	require.Equal(t, value, m.Value())
	require.Equal(t, snapshot, m.Layout())
	for range 3 {
		m.View()
		m.Cursor()
		require.Equal(t, revision, m.Revision())
		require.Equal(t, snapshot, m.Layout())
	}
	detached := m.Layout()
	detached.Rows[0].Runs[0].Text = "tampered"
	require.Equal(t, snapshot, m.Layout())
	m.SetVirtualCursor(false)
	c := m.Cursor()
	c.X = 999
	require.NotEqual(t, c.X, m.Cursor().X)
}
func TestGraphemeSelectionAndCellHits(t *testing.T) {
	m := plainArea(20, 1)
	m.SetValue("first\n界é👩‍💻tail")
	m.Focus()
	require.Equal(t, text.Position{Line: 1, Column: 3}, m.PositionAtCell(0, 4))
	m.SetCursorPosition(m.PositionAtCell(0, 4))
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	require.Equal(t, "👩‍💻", m.SelectedText())
	m, _ = m.Update(tea.PasteMsg{Content: "X"})
	require.Equal(t, "first\n界éXtail", m.Value())
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	require.Equal(t, "first\n界étail", m.Value())
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	require.Equal(t, "first\n界tail", m.Value())
}
func TestLimitsAndAllocatedHeight(t *testing.T) {
	m := plainArea(10, 105)
	require.Equal(t, 105, m.Height())
	require.Equal(t, 99, m.MaxHeight)
	m.MaxHeight = 2
	m.SetValue("a\r\nb\rc")
	require.Equal(t, "a\nb", m.Value())
	m.InsertString("\nc")
	require.Equal(t, "a\nb", m.Value())
	m.CharLimit = 2
	m.SetValue("éX")
	require.Equal(t, "é", m.Value())
	m.CharLimit = 1
	m.SetValue("éX")
	require.Empty(t, m.Value())
	m = plainArea(4, 1)
	m.MaxContentHeight = 1
	m.SetValue("abc")
	m.InsertString("d")
	require.Equal(t, "abc", m.Value())
}
func TestFullStylesAndPlaceholderPadding(t *testing.T) {
	m := plainArea(8, 2)
	s := m.Styles()
	var placeholderSpans []string
	s.Blurred.Text = lipgloss.NewStyle().Transform(func(v string) string { return strings.ReplaceAll(v, "abc", "XYZ") })
	s.Blurred.Placeholder = lipgloss.NewStyle().Transform(func(v string) string { placeholderSpans = append(placeholderSpans, ansi.Strip(v)); return v })
	s.Blurred.Base = lipgloss.NewStyle().Padding(1, 2).Border(lipgloss.NormalBorder())
	m.SetStyles(s)
	m.SetWidth(14)
	m.SetValue("abc")
	require.Contains(t, ansi.Strip(m.View()), "XYZ")
	require.Equal(t, 14, lipgloss.Width(m.View()))
	require.Equal(t, 6, lipgloss.Height(m.View()))
	m.SetValue("")
	m.Placeholder = "hint"
	m.View()
	require.Contains(t, placeholderSpans, "hint")
	for _, span := range placeholderSpans {
		require.Equal(t, strings.TrimRight(span, " "), span, "placeholder style must not color fill cells")
	}
	detached := m.Styles()
	detached.Blurred.Text = detached.Blurred.Text.Bold(true)
	require.False(t, m.Styles().Blurred.Text.GetBold())
}
func TestFocusAndBlink(t *testing.T) {
	m := plainArea(8, 1)
	m.SetValue("abc")
	ignored, _ := m.Update(tea.PasteMsg{Content: "ignored"})
	require.Equal(t, m.Value(), ignored.Value())
	s := m.Styles()
	s.Cursor.BlinkSpeed = time.Millisecond
	m.SetStyles(s)
	m.Focus()
	m, cmd := m.Update(Blink())
	require.NotNil(t, cmd)
	shown := m.View()
	m, next := m.Update(cmd())
	require.NotNil(t, next)
	require.NotEqual(t, shown, m.View())
	revision := m.Revision()
	m, _ = m.Update(cursor.BlinkMsg{})
	require.Equal(t, revision, m.Revision())
	m.Blur()
	hidden := m.View()
	m, _ = m.Update(next())
	require.Equal(t, hidden, m.View())
}
