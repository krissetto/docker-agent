package viewport

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testViewport() Model {
	m := New()
	m.SetWidth(8)
	m.SetHeight(6)
	m.SetContent(strings.Repeat("0123456789abcdefghijklmnop\n", 30))
	return m
}

func TestPagerKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		rows int
		cols int
	}{
		{"down", tea.KeyPressMsg{Code: tea.KeyDown}, 1, 0},
		{"j", tea.KeyPressMsg{Code: 'j'}, 1, 0},
		{"up", tea.KeyPressMsg{Code: tea.KeyUp}, -1, 0},
		{"k", tea.KeyPressMsg{Code: 'k'}, -1, 0},
		{"page down", tea.KeyPressMsg{Code: tea.KeyPgDown}, 6, 0},
		{"space", tea.KeyPressMsg{Code: tea.KeySpace}, 6, 0},
		{"f", tea.KeyPressMsg{Code: 'f'}, 6, 0},
		{"page up", tea.KeyPressMsg{Code: tea.KeyPgUp}, -6, 0},
		{"b", tea.KeyPressMsg{Code: 'b'}, -6, 0},
		{"half down", tea.KeyPressMsg{Code: 'd'}, 3, 0},
		{"ctrl half down", tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, 3, 0},
		{"half up", tea.KeyPressMsg{Code: 'u'}, -3, 0},
		{"ctrl half up", tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}, -3, 0},
		{"right", tea.KeyPressMsg{Code: tea.KeyRight}, 0, 6},
		{"l", tea.KeyPressMsg{Code: 'l'}, 0, 6},
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}, 0, -6},
		{"h", tea.KeyPressMsg{Code: 'h'}, 0, -6},
		{"unbound home", tea.KeyPressMsg{Code: tea.KeyHome}, 0, 0},
		{"unbound end", tea.KeyPressMsg{Code: tea.KeyEnd}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testViewport()
			m.yOffset, m.xOffset = 10, 6
			next, cmd := m.Update(tc.key)
			assert.Nil(t, cmd)
			assert.Equal(t, 10+tc.rows, next.YOffset())
			assert.Equal(t, 6+tc.cols, next.xOffset)
			assert.Equal(t, 10, m.YOffset(), "Update must not mutate the original")
		})
	}
}

func TestWheel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		button tea.MouseButton
		mod    tea.KeyMod
		rows   int
		cols   int
	}{
		{"down", tea.MouseWheelDown, 0, 3, 0},
		{"up", tea.MouseWheelUp, 0, -3, 0},
		{"shift down", tea.MouseWheelDown, tea.ModShift, 0, 6},
		{"shift up", tea.MouseWheelUp, tea.ModShift, 0, -6},
		{"right", tea.MouseWheelRight, 0, 0, 6},
		{"left", tea.MouseWheelLeft, 0, 0, -6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testViewport()
			m.yOffset, m.xOffset = 10, 6
			m, _ = m.Update(tea.MouseWheelMsg{Button: tc.button, Mod: tc.mod})
			assert.Equal(t, 10+tc.rows, m.YOffset())
			assert.Equal(t, 6+tc.cols, m.xOffset)
		})
	}
}

func TestClampingAndResize(t *testing.T) {
	t.Parallel()
	m := testViewport()
	m.ScrollDown(100)
	assert.Equal(t, m.TotalLineCount()-m.Height(), m.YOffset())
	m.ScrollDown(-100)
	assert.Zero(t, m.YOffset())
	m.GotoBottom()
	m.SetHeight(100)
	assert.Zero(t, m.YOffset(), "growing the view must clamp the last page")
	m.SetHeight(6)
	m.GotoBottom()
	m.GotoTop()
	assert.Zero(t, m.YOffset())
	for range 10 {
		m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	assert.Equal(t, m.longestLine-m.width, m.xOffset)
	m.SetWidth(100)
	assert.Zero(t, m.xOffset)
	m.GotoBottom()
	m.SetContent("short\r\ntext")
	assert.Equal(t, "short\ntext", m.GetContent())
	assert.Equal(t, 2, m.TotalLineCount())
	assert.Zero(t, m.YOffset(), "replacing content must clamp the last page")
	m.SetContent("")
	assert.Zero(t, m.TotalLineCount())
	assert.Zero(t, m.YOffset())
	m.SetWidth(-1)
	assert.Empty(t, m.View())
	m.SetWidth(5)
	m.SetHeight(-1)
	assert.Empty(t, m.View())
}

func TestViewClipsANSIAndWideText(t *testing.T) {
	t.Parallel()
	m := New()
	m.SetWidth(6)
	m.SetHeight(3)
	m.SetContent("\x1b[31m你好abcdefghi\x1b[0m\nx")
	view := m.View()
	assert.Contains(t, view, "\x1b[31m")
	assert.Equal(t, "你好ab\nx     \n      ", ansi.Strip(view))
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, "cdefgh\n      \n      ", ansi.Strip(m.View()))
	for _, row := range strings.Split(m.View(), "\n") {
		require.Equal(t, 6, ansi.StringWidth(row))
	}
	m.SetContent("a\n")
	assert.Equal(t, 2, m.TotalLineCount(), "preserve a trailing blank line")
	assert.Equal(t, "a     \n      \n      ", m.View())
}
