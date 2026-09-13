package sidebar

import (
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestQueuedPreviewTwoLinesCanonicalIdentityAndRemoveCell(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	full := "界界 one\nsecond line with more words\nthird line preserved"
	settleTreePresentation(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "canonical-turn", Text: full}}))
	m.SetSize(20, 30)
	m.CancelPresentation()
	m.View()
	rows := m.queueRows
	require.Len(t, rows, 3)
	assert.Equal(t, "queue-header", rows[0].id)
	assert.Equal(t, full, m.queuedMessages[0].Text)
	var first placedRow
	for _, row := range m.placement.rows {
		if row.id == "queue:canonical-turn:0" {
			first = row
		}
	}
	require.NotEmpty(t, first.id)
	before := m.View()
	require.Contains(t, ansi.Strip(before), "Queue:")
	require.Contains(t, ansi.Strip(before), "…")
	bodyY := int(first.y) - m.scrollview.ScrollOffset()
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft + 2, Y: bodyY})
	settleSidebarHover(t, m, cmd)
	result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+2, bodyY)
	assert.Equal(t, ClickQueuedMessage, result)
	assert.Equal(t, "canonical-turn", payload)
	right := m.layoutCfg.PaddingLeft + m.contentWidth(m.cachedNeedsScrollbar) - 1
	_, cmd = m.Update(tea.MouseMotionMsg{X: right, Y: bodyY})
	settleSidebarHover(t, m, cmd)
	after := strings.Split(m.View(), "\n")[bodyY]
	assert.Contains(t, after, styles.ErrorStyle.Render("×"))
	result, payload = m.HandleClickType(right, bodyY)
	assert.Equal(t, ClickRemoveQueuedMessage, result)
	assert.Equal(t, "canonical-turn", payload)
	assert.Equal(t, ansi.StringWidth(strings.Split(before, "\n")[bodyY]), ansi.StringWidth(after))
	headerY := m.queueStart - m.scrollview.ScrollOffset()
	result, _ = m.HandleClickType(m.layoutCfg.PaddingLeft, headerY)
	assert.Equal(t, ClickNone, result, "Queue heading has no edit/remove action")
}

func TestDirectoryFolderIconDedicatedRawPathAction(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.workingDirectory = "/raw path/with spaces/project"
	m.invalidateCache()
	settleTreePresentation(t, m, m.ReconcileLayout())
	m.View()
	y := m.workingDirRow - m.scrollview.ScrollOffset()
	_, cmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
	settleSidebarHover(t, m, cmd)
	require.Contains(t, m.View(), directoryIcon)
	width := m.contentWidth(m.cachedNeedsScrollbar)
	iconX := m.layoutCfg.PaddingLeft + width - directoryIconWidth() - 1
	for dx := range directoryIconWidth() {
		result, payload := m.HandleClickType(iconX+dx, y)
		assert.Equal(t, ClickOpenWorkingDir, result)
		assert.Equal(t, "/raw path/with spaces/project", payload)
	}
	result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft, y)
	assert.Equal(t, ClickWorkingDir, result, "ordinary directory click retains clipboard action")
	line := m.directoryRow(width)
	assert.True(t, strings.HasSuffix(ansi.Strip(line), directoryCopyIcon+" "+directoryIcon+" "), "one icon separator and one whole-line trailing cell")
	result, _ = m.HandleClickType(m.layoutCfg.PaddingLeft+width-1, y)
	assert.Equal(t, ClickNone, result, "final directory margin is inert")
	result, _ = m.HandleClickType(iconX-1, y)
	assert.Equal(t, ClickWorkingDir, result, "space between icons retains row copy policy")
	settleSidebarHover(t, m, m.ClearSubagentHover())
	m.SetSize(3, 8)
	m.CancelPresentation()
	m.View()
	assert.NotContains(t, m.View(), directoryIcon)
	for x := range 3 {
		result, _ = m.HandleClickType(x, m.workingDirRow)
		assert.NotEqual(t, ClickOpenWorkingDir, result, "clipped icon never leaves a dead action")
	}
}

func TestDirectoryArrowFadesAtStableSingleCell(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	width := 30
	original := styles.MutedStyle.Render(directoryIcon)
	require.Equal(t, 1, directoryIconWidth())
	var baseName string
	for _, progress := range []float64{0, .5, 1, .5, 0} {
		m.hoverValues = map[string]hoverValue{"directory": {value: progress, target: progress}, "directory-open": {value: progress, target: progress}}
		line := m.directoryRow(width)
		cells := sidebarCells(line)
		require.Len(t, cells, width)
		arrow := cells[width-2]
		assert.Equal(t, directoryIcon, arrow.glyph)
		expected := sidebarCells(styles.FadeLine(styles.HoverText(original, progress, styles.TextPrimary), progress))
		require.Len(t, expected, 1)
		assert.Equal(t, color.NRGBAModel.Convert(expected[0].fg), color.NRGBAModel.Convert(arrow.fg))
		assert.Nil(t, arrow.bg, "arrow has no box/background fill")
		name := ansi.Strip(ansi.Cut(line, 0, width-directoryReserve()))
		if baseName == "" {
			baseName = name
		} else {
			assert.Equal(t, baseName, name, "entry/exit arrow never shifts path")
		}
	}
}

func TestDirectoryGroupedRowHoverAndIndependentIconEmphasis(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.View()
	y := m.workingDirRow - m.scrollview.ScrollOffset()
	width := m.contentWidth(m.cachedNeedsScrollbar)
	arrowX := m.layoutCfg.PaddingLeft + width - 2
	copyX := arrowX - 2
	labelX := m.layoutCfg.PaddingLeft
	_, cmd := m.Update(tea.MouseMotionMsg{X: labelX, Y: y})
	settleSidebarHover(t, m, cmd)
	require.InDelta(t, 1, m.hoverValues["directory"].value, 0)
	require.InDelta(t, 1, m.hoverValues["directory-copy"].value, 0)
	require.Zero(t, m.hoverValues["directory-open"].value)
	for _, x := range []int{copyX, arrowX, copyX} {
		before := m.hoverValues["directory"].value
		_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: y})
		require.InDelta(t, before, m.hoverValues["directory"].value, 0, "zone motion cannot restart row hover")
		require.InDelta(t, 1, m.hoverValues["directory"].target, 0)
		settleSidebarHover(t, m, cmd)
		cells := sidebarCells(m.directoryRow(width))
		copyBase := styles.MutedStyle.GetForeground()
		arrowBase := copyBase
		if x == arrowX {
			arrowBase = styles.Brighten(arrowBase, .25)
		} else {
			copyBase = styles.Brighten(copyBase, .25)
		}
		assert.Equal(t, color.NRGBAModel.Convert(copyBase), color.NRGBAModel.Convert(cells[width-4].fg))
		assert.Equal(t, color.NRGBAModel.Convert(arrowBase), color.NRGBAModel.Convert(cells[width-2].fg))
		result, payload := m.HandleClickType(x, y)
		if x == arrowX {
			assert.Equal(t, ClickOpenWorkingDir, result)
			assert.Equal(t, m.WorkingDirectory(), payload)
		} else {
			assert.Equal(t, ClickWorkingDir, result)
		}
	}
	settleSidebarHover(t, m, m.ClearSubagentHover())
	assert.Zero(t, m.ar.ActiveCount())
	cells := sidebarCells(m.directoryRow(width))
	for _, x := range []int{width - 4, width - 2} {
		assert.Equal(t, color.NRGBAModel.Convert(styles.Background), color.NRGBAModel.Convert(cells[x].fg), "all icons fade out with the row")
	}
}
