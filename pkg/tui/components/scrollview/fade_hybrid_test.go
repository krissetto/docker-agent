package scrollview

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertViewport(t *testing.T, out string, width, height int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	require.Len(t, lines, height)
	for i, line := range lines {
		assert.Equalf(t, width, ansi.StringWidth(line), "row %d", i)
	}
}

func TestFadeDefaultsOnAndCanBeDisabled(t *testing.T) {
	content := []string{"zero", "one", "two", "three", "four", "five"}
	plain := New()
	plain.SetSize(16, 4)
	plain.SetContent(content, len(content))
	assert.True(t, plain.fadeEffect)

	disabled := New(WithFadeEffectDisabled())
	disabled.SetSize(16, 4)
	disabled.SetContent(content, len(content))
	assert.False(t, disabled.fadeEffect)
	assert.NotEqual(t, disabled.View(), plain.View())

	faded := New(WithFadeEffect())
	faded.SetSize(16, 4)
	faded.SetContent(content, len(content))
	top := strings.Split(faded.View(), "\n")
	assert.False(t, strings.HasPrefix(top[0], "\x1b[38;2;"), "no content exists above")
	assert.True(t, strings.HasPrefix(top[len(top)-1], "\x1b[38;2;"), "content exists below")

	faded.SetScrollOffset(1)
	middle := strings.Split(faded.View(), "\n")
	assert.True(t, strings.HasPrefix(middle[0], "\x1b[38;2;"))
	assert.True(t, strings.HasPrefix(middle[len(middle)-1], "\x1b[38;2;"))

	faded.ScrollToBottom()
	bottom := strings.Split(faded.View(), "\n")
	assert.True(t, strings.HasPrefix(bottom[0], "\x1b[38;2;"))
	assert.False(t, strings.HasPrefix(bottom[len(bottom)-1], "\x1b[38;2;"), "no content exists below")
}

func TestRestyledViewTracksMutableCallerLines(t *testing.T) {
	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(20, 4)
	content := []string{"zero", "one", "two", "three", "four", "five"}
	m.SetContent(content, len(content))
	visible := append([]string(nil), content[:4]...)

	first := m.ViewWithRestyledLines(visible)
	visible[1] = "changed"
	assert.NotEqual(t, first, m.ViewWithRestyledLines(visible))
	assert.Equal(t, "one", content[1])

	m.SetScrollOffset(1)
	middle := m.ViewWithRestyledLines(content[1:5])
	assert.NotEqual(t, first, middle)

	m.SetSize(18, 4)
	assert.NotEqual(t, middle, m.ViewWithRestyledLines(content[1:5]))
}

func TestViewsDoNotMutateCallerSlices(t *testing.T) {
	m := New(WithFadeEffect(), WithReserveScrollbarSpace(true))
	m.SetSize(18, 4)
	m.SetContent([]string{"a", "b", "c", "d", "e"}, 5)
	m.SetScrollOffset(1)

	for name, view := range map[string]func([]string) string{
		"lines":    m.ViewWithLines,
		"restyled": m.ViewWithRestyledLines,
		"padded":   m.ViewWithPaddedLines,
	} {
		t.Run(name, func(t *testing.T) {
			input := []string{"one", "two", "three", "four"}
			before := append([]string(nil), input...)
			_ = view(input)
			assert.Equal(t, before, input)
		})
	}
}

func TestExactViewportANSIWideResizeAndReservedSpace(t *testing.T) {
	m := New(WithGapWidth(2), WithReserveScrollbarSpace(true), WithFadeEffect())
	m.SetSize(14, 3)
	content := []string{"\x1b[31m界🙂abcdef\x1b[m", "short", "wide界", "tail"}
	m.SetContent(content, len(content))
	assertViewport(t, m.View(), 14, 3)

	m.SetSize(9, 5)
	assertViewport(t, m.View(), 9, 5)
	assert.Equal(t, 3, m.ReservedCols())
	assert.Equal(t, 6, m.ContentWidth())
}

func TestPaddedLinesAndIntegratedOuterPadding(t *testing.T) {
	m := New(WithGapWidth(0), WithReserveScrollbarSpace(true), WithFadeEffect())
	m.SetSize(8, 3)
	m.SetContent([]string{"1234567", "abcdefg", "界界界x", "last"}, 4)
	m.SetScrollOffset(1)

	padded := []string{"abcdefg", "界界界x", "last   "}
	before := append([]string(nil), padded...)
	assertViewport(t, m.ViewWithPaddedLines(padded), 8, 3)
	assert.Equal(t, before, padded)

	out := m.ViewWithPaddedLinesAndPadding(padded, 2, 3)
	assertViewport(t, out, 13, 3)
	assert.Equal(t, before, padded)
}

func TestPaddedLinesAndPaddingTreatsViewportSizedInputAsPreSliced(t *testing.T) {
	m := New(WithGapWidth(0))
	m.SetSize(3, 3)
	m.SetContent([]string{"stored-a", "stored-b", "stored-c"}, 10)
	m.SetScrollOffset(2)

	out := m.ViewWithPaddedLinesAndPadding([]string{"x", "y", "z"}, 0, 0)
	lines := strings.Split(ansi.Strip(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []byte{'x', 'y', 'z'}, []byte{lines[0][0], lines[1][0], lines[2][0]})
}

func TestPaddedContentAndPaddingSlicesFullBuffer(t *testing.T) {
	m := New(WithGapWidth(0))
	m.SetSize(3, 3)
	content := []string{"a", "b", "x", "y", "z", "tail"}
	m.SetContent(content, 10)
	m.SetScrollOffset(2)

	out := m.ViewWithPaddedContentAndPadding(content, 0, 0)
	lines := strings.Split(ansi.Strip(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []byte{'x', 'y', 'z'}, []byte{lines[0][0], lines[1][0], lines[2][0]})
}

func TestMetricsHitTestingInvalidationAndDecoration(t *testing.T) {
	m := New(WithGapWidth(2), WithScrollbarDecorator(func(s string) string {
		return strings.ReplaceAll(s, "│", "!")
	}))
	m.SetPosition(10, 20)
	m.SetSize(12, 3)
	content := []string{"a", "b", "c", "d", "e"}
	m.SetContent(content, len(content))

	assert.Equal(t, 2, m.MaxScrollOffset())
	assert.False(t, m.IsAtBottom())
	assert.True(t, m.IsMouseOnScrollbar(21, 20))
	assert.False(t, m.IsMouseOnScrollbar(20, 20))
	assert.Contains(t, m.View(), "!")

	m.ScrollToBottom()
	assert.True(t, m.IsAtBottom())

	content[2] = "changed in place"
	m.SetContent(append([]string(nil), content...), len(content))
	assert.Contains(t, ansi.Strip(m.View()), "chang")
}

func TestFadeDepthScalesAndCaps(t *testing.T) {
	assert.Equal(t, MinFadeLines, fadeLinesForHeight(1))
	assert.Greater(t, fadeLinesForHeight(40), fadeLinesForHeight(4))
	assert.Equal(t, MaxFadeLines, fadeLinesForHeight(1000))
}
