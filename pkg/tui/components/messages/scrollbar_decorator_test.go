package messages

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestSetScrollbarDecoratorRestylesOnlyScrollbarColumn pins that the
// decorator reaches the composed frame (bypassing the identical-frame cache)
// while leaving the prepared transcript and every content cell untouched.
func TestSetScrollbarDecoratorRestylesOnlyScrollbarColumn(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 30, 5, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	addShrinkingView(m, 30)
	before := m.View()
	require.True(t, m.scrollview.NeedsScrollbar())
	generation := m.VisualGeneration()
	rebuilds, misses, rendered := m.ResizeCacheStats()

	m.SetScrollbarDecorator(func(view string) string {
		return styles.FadeLine(view, 0.2)
	})
	require.NotEqual(t, generation, m.VisualGeneration(), "root view cache must recompose")
	after := m.View()
	require.NotEqual(t, before, after)
	require.Equal(t, after, m.View(), "decorated frame is retained like any other")

	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	require.Len(t, afterLines, len(beforeLines))
	column := m.scrollview.ScrollbarX() - m.xPos
	for i := range beforeLines {
		require.Equal(t, ansi.Strip(beforeLines[i]), ansi.Strip(afterLines[i]), "row %d glyphs unchanged", i)
		require.Equal(t, 30, ansi.StringWidth(afterLines[i]), "row %d width unchanged", i)
		require.Equal(t, contentCells(beforeLines[i], column), contentCells(afterLines[i], column), "row %d content cells unchanged", i)
	}

	r2, m2, n2 := m.ResizeCacheStats()
	require.Equal(t, [3]uint64{rebuilds, misses, rendered}, [3]uint64{r2, m2, n2}, "decorating never re-prepares the transcript")

	generation = m.VisualGeneration()
	m.SetScrollbarDecorator(nil)
	require.NotEqual(t, generation, m.VisualGeneration())
	require.Equal(t, before, m.View(), "clearing restores the undecorated frame")

	generation = m.VisualGeneration()
	m.SetScrollbarDecorator(nil)
	require.Equal(t, generation, m.VisualGeneration(), "nil over nil is a no-op")
	require.NotEmpty(t, m.lastFrameOutput)
}

var trailingSGR = regexp.MustCompile(`(\x1b\[[0-9;:]*m)+$`)

// contentCells returns the first width cells of line without the styling that
// ansi.Truncate carries over from the following cell.
func contentCells(line string, width int) string {
	return trailingSGR.ReplaceAllString(ansi.Truncate(line, width, ""), "")
}
