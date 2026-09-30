package messagebar

import (
	"image/color"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func notice() Message {
	return Message{Text: "Awaiting a short decision", Actions: []Action{
		{Label: "Open", Command: func() tea.Msg { return messages.SpawnSessionMsg{} }},
		{Label: "Dismiss", Command: func() tea.Msg { return messages.CloseTabMsg{SessionID: "notice"} }},
	}}
}

func TestWidthsAndStableIdleRow(t *testing.T) {
	for _, width := range []int{0, 1, 2, 4, 8, 20, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := New()
			assert.Nil(t, m.SetSize(width, 1))
			assert.Equal(t, strings.Repeat(" ", width), ansi.Strip(m.View()))
			assert.Empty(t, strings.TrimSpace(ansi.Strip(m.View())))
			m.SetMessage(notice())
			assert.Equal(t, 1, m.Height())
			view := ansi.Strip(m.View())
			assert.Equal(t, width, ansi.StringWidth(view))
			assert.NotContains(t, view, "\n")
			for x := -1; x <= width; x++ {
				index, hit := m.HitTest(x, 0)
				if !hit {
					assert.Equal(t, -1, index)
					continue
				}
				assert.GreaterOrEqual(t, x, 0)
				assert.Less(t, x, width)
				assert.Less(t, index, len(m.message.Actions))
			}
			if width < 8 {
				assert.False(t, m.HasActions())
			}
		})
	}
}

func TestCompleteActionsTakePriorityOverLongMessage(t *testing.T) {
	m := New()
	m.SetSize(18, 1)
	m.SetMessage(notice())
	require.Len(t, m.bounds, 2)
	view := ansi.Strip(m.View())
	assert.Equal(t, 18, ansi.StringWidth(view))
	assert.Contains(t, view, " Open ")
	assert.Contains(t, view, " Dismiss ")
	assert.Equal(t, m.width, m.bounds[len(m.bounds)-1].end)
	assert.True(t, strings.HasPrefix(view, "…"), "the component must not add an internal left inset")
	_, hit := m.HitTest(m.width-1, 0)
	assert.True(t, hit, "the root, not the component, owns the outer right margin")
	assert.Equal(t, "…", m.text)
}

func TestStableBlankRowAndAllocatedHeight(t *testing.T) {
	m := New()
	m.SetSize(20, 4)
	assert.Equal(t, 1, m.Height())
	assert.Equal(t, strings.Repeat(" ", 20), ansi.Strip(m.View()))
	m.SetMessage(notice())
	m.ClearMessage()
	assert.Equal(t, strings.Repeat(" ", 20), ansi.Strip(m.View()))
	assert.False(t, m.HasActions())
	m.SetSize(20, 0)
	assert.Zero(t, m.Height())
	assert.Empty(t, m.View())
	m.SetSize(-1, -1)
	assert.Zero(t, m.Height())
	assert.Empty(t, m.View())
}

func TestActionHitBoundsAndCommandsBeforeView(t *testing.T) {
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(notice())
	require.Len(t, m.bounds, 2)
	for _, bounds := range m.bounds {
		for x := bounds.start; x < bounds.end; x++ {
			index, ok := m.HitTest(x, 0)
			require.True(t, ok)
			assert.Equal(t, bounds.index, index)
		}
		_, ok := m.HitTest(bounds.end, 0)
		assert.False(t, ok)
		_, ok = m.HitTest(bounds.start, 1)
		assert.False(t, ok)
	}
	first := m.bounds[0]
	cmd := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: first.start, Y: 0})
	require.NotNil(t, cmd)
	assert.IsType(t, messages.SpawnSessionMsg{}, cmd())
	assert.False(t, m.Focused(), "click activation must not capture keyboard focus")
	assert.Nil(t, m.Update(tea.MouseClickMsg{Button: tea.MouseRight, X: first.start, Y: 0}))
	assert.Nil(t, m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: m.width, Y: 0}))
	m.SetSize(4, 1)
	_, ok := m.HitTest(first.start, 0)
	assert.False(t, ok, "resize invalidates geometry before View")
}

func TestKeyboardRequiresExplicitFocusAndVisibleActions(t *testing.T) {
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(notice())
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	assert.Nil(t, m.Update(enter))
	m.SetFocused(true)
	require.True(t, m.Focused())
	cmd := m.Update(enter)
	require.NotNil(t, cmd)
	assert.IsType(t, messages.SpawnSessionMsg{}, cmd())
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	cmd = m.Update(enter)
	require.NotNil(t, cmd)
	assert.Equal(t, messages.CloseTabMsg{SessionID: "notice"}, cmd())
	assert.Nil(t, m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"}), "no global action-letter bindings")
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, 0, m.selected)
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.Equal(t, 1, m.selected)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.False(t, m.Focused())
	m.SetFocused(true)
	m.SetSize(4, 1)
	assert.False(t, m.Focused())
	assert.Nil(t, m.Update(enter))
	m.ClearMessage()
	m.SetFocused(true)
	assert.False(t, m.Focused())
}

func TestEventsCopyMessagesAndResetInteraction(t *testing.T) {
	m := New()
	m.SetSize(80, 1)
	message := notice()
	assert.Nil(t, m.Update(SetMessageMsg(message)))
	message.Actions[0].Label = "Changed outside event loop"
	assert.Contains(t, ansi.Strip(m.View()), "Open")
	assert.NotContains(t, ansi.Strip(m.View()), "Changed")
	m.SetFocused(true)
	m.Update(tea.MouseMotionMsg{X: m.bounds[0].start, Y: 0})
	assert.Nil(t, m.Update(ClearMessageMsg{}))
	assert.False(t, m.Focused())
	assert.False(t, m.HasActions())
	assert.Equal(t, -1, m.hovered)
	assert.Equal(t, strings.Repeat(" ", 80), ansi.Strip(m.View()))
}

func TestSingleRowUnicodeAndNoPartialActions(t *testing.T) {
	for _, width := range []int{0, 1, 2, 4, 8, 20, 80} {
		m := New()
		m.SetSize(width, 1)
		m.SetMessage(Message{Text: "\x1b[31mWorking\x1b[0m\r\n界界界\tmessage", Actions: []Action{
			{Label: "界\nOpen", Command: func() tea.Msg { return messages.SpawnSessionMsg{} }},
		}})
		view := ansi.Strip(m.View())
		assert.Equal(t, width, ansi.StringWidth(view))
		assert.NotContains(t, view, "\n")
		assert.NotContains(t, view, "\r")
		assert.NotContains(t, view, "\t")
		for _, bounds := range m.bounds {
			assert.Equal(t, " 界 Open ", ansi.Cut(view, bounds.start, bounds.end))
			assert.Equal(t, ansi.StringWidth(" 界 Open "), bounds.end-bounds.start)
		}
	}
}

func TestNilAndEmptyActionsAreNotFocusable(t *testing.T) {
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(Message{Actions: []Action{
		{Label: "Not actionable"},
		{Command: func() tea.Msg { return messages.SpawnSessionMsg{} }},
	}})
	assert.False(t, m.HasActions())
	m.SetFocused(true)
	assert.False(t, m.Focused())
}

func TestHoverUsesSharedForegroundAndNoOpMotionStaysClean(t *testing.T) {
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(notice())
	baseline := m.View()
	m.TakeVisualDirty()
	bounds := m.bounds[0]
	motion := tea.MouseMotionMsg{X: bounds.start, Y: 0}
	assert.Nil(t, m.Update(motion))
	assert.True(t, m.TakeVisualDirty())
	view := m.View()
	pill := styles.NoStyle.Padding(0, 1).Bold(true).Foreground(styles.TextPrimary).Render("Open")
	assert.Contains(t, view, styles.HoverText(pill, 1, styles.TextPrimary))
	assert.Equal(t, ansi.Strip(baseline), ansi.Strip(view))
	assert.Nil(t, m.Update(motion))
	assert.False(t, m.TakeVisualDirty())
	assert.Equal(t, view, m.View())
	m.Update(tea.MouseMotionMsg{X: bounds.end, Y: 0})
	assert.True(t, m.TakeVisualDirty())
	assert.Equal(t, baseline, m.View())
	m.Update(tea.MouseMotionMsg{X: -1, Y: -1})
	assert.False(t, m.TakeVisualDirty())
}

func TestWarmViewAndThemeGeneration(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New()
	m.SetSize(80, 1)
	m.SetMessage(notice())
	assert.Zero(t, testing.AllocsPerRun(20, func() { _ = m.View() }))
	view := m.View()
	oldGeneration := m.theme
	styles.ApplyTheme(styles.DefaultTheme())
	assert.Greater(t, styles.ThemeGeneration(), oldGeneration)
	assert.Equal(t, ansi.Strip(view), ansi.Strip(m.View()))
	assert.Equal(t, m.render(), m.View(), "a stale cache must render the current palette")
	assert.Equal(t, oldGeneration, m.theme, "View must not refresh the cache")
	assert.Equal(t, view, m.cachedView)
	assert.Nil(t, m.Update(struct{}{}))
	assert.Equal(t, styles.ThemeGeneration(), m.theme)
	assert.True(t, m.cacheValid)
	assert.Zero(t, testing.AllocsPerRun(20, func() { _ = m.View() }))
}

func TestNoticeForegroundAfterThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New()
	m.SetSize(20, 1)
	m.SetMessage(Message{Text: "Warning", Severity: Warning})
	contextColor := styles.ContextEmpty
	fg := foregroundAt(t, "\x1b[38;2;0;0;255m"+m.View(), "W")
	assert.Equal(t, rgba(styles.Warning), rgba(fg))
	assert.Equal(t, contextColor, styles.ContextEmpty)
	theme := *styles.DefaultTheme()
	theme.Colors.Warning = "#ddaa33"
	styles.ApplyTheme(&theme)
	generation := m.theme
	fresh := m.View()
	assert.Equal(t, rgba(styles.Warning), rgba(foregroundAt(t, fresh, "W")))
	assert.Equal(t, generation, m.theme, "theme fallback must keep View pure")
	m.Update(struct{}{})
	assert.Equal(t, fresh, m.View())
	assert.Equal(t, styles.ThemeGeneration(), m.theme)
}

func rgba(c color.Color) color.RGBA64 {
	return color.RGBA64Model.Convert(c).(color.RGBA64)
}

func foregroundAt(t *testing.T, text, glyph string) color.Color {
	t.Helper()
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var foreground color.Color
	for text != "" {
		seq, width, n, next := ansi.DecodeSequence(text, state, parser)
		require.Positive(t, n)
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			params := parser.Params()
			if len(params) == 0 {
				foreground = nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); param {
				case 0, 39:
					foreground = nil
				case 38, 48, 58:
					var c color.Color
					if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
						if param == 38 {
							foreground = c
						}
						i += consumed - 1
					}
				}
			}
		}
		if width > 0 && seq == glyph {
			require.NotNil(t, foreground)
			return foreground
		}
		state, text = next, text[n:]
	}
	t.Fatalf("glyph %q not rendered", glyph)
	return nil
}

func assertDefaultBackground(t *testing.T, view string, width int) {
	t.Helper()
	rows := uv.NewStyledString(view).Lines(ansi.GraphemeWidth)
	require.Len(t, rows, 1)
	require.Equal(t, width, ansi.StringWidth(view))
	for x, cell := range rows[0] {
		require.Nil(t, cell.Style.Bg, "messagebar cell %d stays terminal default", x)
	}
}

func TestDefaultBackgroundAcrossThemesSeveritiesAndActions(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New()
	m.SetSize(80, 1)
	for _, ref := range []string{"default", "default-light", "gruvbox-dark"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		assertDefaultBackground(t, m.View(), 80)
		for _, severity := range []Severity{Info, Warning, Error, Success} {
			m.ClearMessage()
			m.SetMessage(Message{Text: "Notice 界 é 👩‍💻 https://example.test", Severity: severity})
			assertDefaultBackground(t, m.View(), 80)
			assert.Equal(t, rgba(m.severityColor()), rgba(foregroundAt(t, m.View(), "N")))
			assert.Contains(t, ansi.Strip(m.View()), "界 é 👩‍💻 https://example.test")
		}
		m.ClearMessage()
		m.SetMessage(notice())
		assertDefaultBackground(t, m.View(), 80)
		m.SetFocused(true)
		assertDefaultBackground(t, m.View(), 80)
		cells := uv.NewStyledString(m.View()).Lines(ansi.GraphemeWidth)[0]
		assert.NotZero(t, cells[m.bounds[0].start+1].Style.Underline, "keyboard selection remains visible")
		m.Update(tea.MouseMotionMsg{X: m.bounds[0].start, Y: 0})
		assertDefaultBackground(t, m.View(), 80)
		m.ClearMessage()
		assertDefaultBackground(t, m.View(), 80)
	}
}
