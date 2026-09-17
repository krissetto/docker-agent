package sidebar

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestCompactBandBoundsEveryFieldAndHitTarget(t *testing.T) {
	for _, width := range []int{0, 1, 2, 8, 30, 80, 160} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			vm := CollapsedViewModel{
				TitleWithStar:    strings.Repeat("界 title\n", 30),
				WorkingIndicator: "working\nagain",
				WorkingDir:       "/workspace\nextra",
				Branch:           "main\nextra",
				ModelInfo:        "model\nprovider\nextra",
				UsageSummary:     "tokens $0.00",
				InfoLine:         "root · helper\nextra",
				Yolo:             "YOLO",
				ContentWidth:     width,
			}
			view := RenderCollapsedView(vm)
			lines := strings.Split(view, "\n")
			require.Len(t, lines, vm.LineCount())
			require.LessOrEqual(t, len(lines), 2)
			for _, line := range lines {
				require.LessOrEqual(t, ansi.StringWidth(line), width)
			}
			for _, span := range vm.spans() {
				require.GreaterOrEqual(t, span.x, 0)
				require.LessOrEqual(t, span.x+ansi.StringWidth(span.text), width)
				require.Contains(t, ansi.Strip(lines[span.y]), ansi.Strip(span.text))
			}
		})
	}
}

func TestSidebarRepeatedGeometryDoesNotInvalidateWarmContent(t *testing.T) {
	m := newVisibilityTestSidebar(t).model
	m.SetMode(ModeVertical)
	m.SetSize(50, 30)
	m.View()
	invalidations, renders := m.CacheStats()
	generation := m.VisualGeneration()
	for range 20 {
		m.SetMode(ModeVertical)
		m.SetSize(50, 30)
		m.SetPosition(0, 0)
		m.View()
	}
	i, r := m.CacheStats()
	require.Equal(t, invalidations, i)
	require.Equal(t, renders, r)
	require.Equal(t, generation, m.VisualGeneration())
	m.SetMode(ModeCollapsed)
	m.View()
	i, r = m.CacheStats()
	require.Greater(t, i, invalidations)
	require.Greater(t, r, renders)
}
