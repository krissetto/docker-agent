package scrollview

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestFadeCacheReusesEdgesAndTracksVisibleChanges(t *testing.T) {
	m := New()
	m.SetSize(24, 10)
	content := make([]string, 30)
	for i := range content {
		content[i] = fmt.Sprintf("\x1b[1;38;2;100;180;220mline %02d界\x1b[m", i)
	}
	m.SetContent(content, len(content))
	m.SetScrollOffset(5)
	visible := append([]string(nil), content[5:15]...)
	first := m.ViewWithRestyledLines(visible)
	top := m.fadeCache.top[0].output
	bottom := m.fadeCache.bottom[0].output
	assert.Equal(t, first, m.ViewWithRestyledLines(visible))
	assert.Same(t, unsafe.StringData(top), unsafe.StringData(m.fadeCache.top[0].output), "idle edge must reuse cached string")
	assert.Same(t, unsafe.StringData(bottom), unsafe.StringData(m.fadeCache.bottom[0].output))

	visible[0] = "\x1b[48;2;90;60;30mselected界\x1b[m"
	changed := m.ViewWithRestyledLines(visible)
	assert.NotEqual(t, first, changed)
	assert.Contains(t, m.fadeCache.top[0].output, "48;2;")
	assert.Same(t, unsafe.StringData(bottom), unsafe.StringData(m.fadeCache.bottom[0].output), "unchanged opposite edge stays cached")
	assert.Equal(t, 5, m.ScrollOffset())
	assertViewport(t, changed, 24, 10)

	beforeResize := m.fadeCache.top[0]
	m.SetSize(18, 20)
	assertViewport(t, m.View(), 18, 20)
	assert.NotEqual(t, beforeResize.alpha, m.fadeCache.top[0].alpha)
	assert.NotEqual(t, beforeResize.line, m.fadeCache.top[0].line)
	assert.Equal(t, 5, m.ScrollOffset())
	assert.LessOrEqual(t, len(m.fadeCache.top), MaxFadeLines)
}

func TestFadeCacheTracksThemeAndExplicitInvalidation(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New()
	m.SetSize(18, 4)
	content := []string{"a", "b", "c", "d", "e", "f"}
	m.SetContent(content, len(content))
	m.SetScrollOffset(1)
	first := m.View()
	palette := m.fadeCache.context
	theme := *original
	theme.Colors.Background = "#ffffff"
	theme.Colors.TextPrimary = "#000000"
	styles.ApplyTheme(&theme)
	assert.NotEqual(t, first, m.View())
	assert.NotEqual(t, palette, m.fadeCache.context)

	content[1] = "changed in place"
	m.InvalidateComposeCache()
	assert.Nil(t, m.lineWidths)
	assert.False(t, m.fadeCache.valid)
	out := m.View()
	assert.Contains(t, ansi.Strip(out), "changed in place")
	assertViewport(t, out, 18, 4)
}

func TestPaddedFadeSharesCacheAndClampsBeforeSlicing(t *testing.T) {
	m := New(WithGapWidth(0))
	m.SetSize(8, 4)
	content := []string{"aaaaaaa", "bbbbbbb", "ccccccc", "ddddddd", "eeeeeee", "fffffff"}
	m.SetContent(content, len(content))
	m.SetScrollOffset(1)
	first := m.ViewWithPaddedLines(content[1:5])
	top := m.fadeCache.top[0].output
	assert.Equal(t, first, m.ViewWithPaddedLinesAndPadding(content[1:5], 0, 0))
	assert.Same(t, unsafe.StringData(top), unsafe.StringData(m.fadeCache.top[0].output), "all rendering paths share cached edges")
	m.ScrollToBottom()
	m.SetSize(8, 5)
	out := m.ViewWithPaddedContentAndPadding(content, 2, 1)
	assert.Equal(t, 1, m.ScrollOffset())
	assertViewport(t, out, 11, 5)
	assert.True(t, strings.HasPrefix(ansi.Strip(out), "  bbbbbbb"), "slice must use offset clamped after resize")
}

func TestFadeIdleProducesNoCommandsOrStateChanges(t *testing.T) {
	m := New()
	m.SetSize(16, 4)
	m.SetContent([]string{"a", "b", "c", "d", "e", "f"}, 6)
	m.SetPosition(10, 20)
	m.SetScrollOffset(1)
	first := m.View()
	before := m.fadeCache.top[0].output
	for range 20 {
		handled, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
		assert.False(t, handled)
		assert.Nil(t, cmd)
		assert.Equal(t, first, m.View())
	}
	assert.Same(t, unsafe.StringData(before), unsafe.StringData(m.fadeCache.top[0].output))
	assert.Equal(t, 1, m.ScrollOffset())
	assert.True(t, m.IsMouseOnScrollbar(25, 20))
	assert.False(t, m.IsMouseOnScrollbar(25, 24))
	m.SetSize(12, 5)
	_ = m.View()
	assert.True(t, m.IsMouseOnScrollbar(21, 24))
	assert.False(t, m.IsMouseOnScrollbar(25, 20))
}

func TestPaddedShortViewportPadsEveryRow(t *testing.T) {
	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(20, 5)
	m.SetContent([]string{"short"}, 1)
	out := m.ViewWithPaddedLines([]string{"short" + strings.Repeat(" ", m.ContentWidth()-5)})
	assertViewport(t, out, 20, 5)
	require.Len(t, strings.Split(out, "\n"), 5)
}

func BenchmarkScrollFade(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		for _, moving := range []bool{false, true} {
			b.Run(fmt.Sprintf("enabled=%t/moving=%t", enabled, moving), func(b *testing.B) {
				m := New()
				m.fadeEffect = enabled
				m.SetSize(100, 40)
				content := make([]string, 200)
				for i := range content {
					content[i] = fmt.Sprintf("\x1b[1;38;2;100;180;220m%03d %s\x1b[m", i, strings.Repeat("界abc", 15))
				}
				m.SetContent(content, len(content))
				m.SetScrollOffset(20)
				_ = m.View()
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					if moving {
						m.SetScrollOffset(20 + i%40)
					}
					_ = m.View()
				}
			})
		}
	}
}
