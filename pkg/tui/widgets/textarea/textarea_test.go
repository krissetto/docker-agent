package textarea

import (
	uv "github.com/charmbracelet/ultraviolet"
	"image/color"
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

func TestAdvertisedWordActions(t *testing.T) {
	for _, tc := range []struct {
		name, value, key, want string
		column                 int
	}{
		{"lowercase skips spaces", "  HELLO next", "l", "  hello next", 7},
		{"uppercase skips spaces", "  hello next", "u", "  HELLO next", 7},
		{"capitalize preserves remainder", "  hELLO next", "c", "  HELLO next", 7},
		{"capitalize punctuation only", "  'hello next", "c", "  'hello next", 8},
		{"forward stops at word end", "  hello next", "f", "  hello next", 7},
		{"delete skips spaces and word", "  hello next", "d", " next", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := plainArea(30, 2)
			m.SetValue(tc.value)
			m.MoveToBegin()
			m.Focus()
			m, _ = m.Update(tea.KeyPressMsg{Code: []rune(tc.key)[0], Mod: tea.ModAlt})
			require.Equal(t, tc.want, m.Value())
			require.Equal(t, tc.column, m.Column())
		})
	}
	m := plainArea(30, 2)
	m.SetValue("  HELLO")
	m.MoveToBegin()
	m.Focus()
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt | tea.ModShift})
	require.Equal(t, "  HELLO", m.SelectedText())
}
func TestCopyWithoutSelectionDoesNotTouchClipboard(t *testing.T) {
	m := plainArea(20, 1)
	m.SetValue("abc")
	m.Focus()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift})
	require.Nil(t, cmd)
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift})
	require.NotNil(t, cmd)
}
func TestSetValueObeysVisualLimit(t *testing.T) {
	m := plainArea(10, 2)
	m.MaxContentHeight = 2
	m.SetValue("one\ntwo\nthree")
	require.Equal(t, "one\ntwo", m.Value())
	require.LessOrEqual(t, m.ContentLineCount(), 2)
	m.SetValue(strings.Repeat("x", 30))
	require.Empty(t, m.Value(), "an oversized single insertion is rejected without truncating a grapheme")
}

func TestPlaceholderForegroundSurvivesCursorReset(t *testing.T) {
	m := plainArea(12, 1)
	m.Placeholder = "Type here"
	m.Focus()
	s := m.Styles()
	s.Focused.Base = lipgloss.NewStyle().Foreground(lipgloss.Color("#c0c0c0")).Background(lipgloss.Color("#25252c"))
	s.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color("#808080"))
	s.Cursor.Color = lipgloss.Color("#ff0000")
	s.Cursor.Blink = false
	m.SetStyles(s)
	cells := uv.NewStyledString(m.View()).Lines(ansi.GraphemeWidth)[0]
	for i := 1; i < len(m.Placeholder); i++ {
		require.Equal(t, color.RGBA{R: 128, G: 128, B: 128, A: 255}, color.RGBAModel.Convert(cells[i].Style.Fg), "placeholder cell %d", i)
	}
	for i := len(m.Placeholder); i < m.Width(); i++ {
		require.Equal(t, color.RGBA{R: 192, G: 192, B: 192, A: 255}, color.RGBAModel.Convert(cells[i].Style.Fg), "padding cell %d", i)
	}
}
