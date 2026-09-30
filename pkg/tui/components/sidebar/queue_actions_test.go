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
	assert.Contains(t, ansi.Strip(after), "✎ ×")
	assert.NotEqual(t, strings.Split(before, "\n")[bodyY], after)
	assert.False(t, m.ConfirmQueuedRemoval("canonical-turn"))
	assert.Equal(t, "canonical-turn", m.queueRemoveArmed)
	assert.True(t, m.ConfirmQueuedRemoval("canonical-turn"))
	assert.Empty(t, m.queueRemoveArmed)
	result, payload = m.HandleClickType(right, bodyY)
	assert.Equal(t, ClickRemoveQueuedMessage, result)
	assert.Equal(t, "canonical-turn", payload)
	assert.Equal(t, ansi.StringWidth(strings.Split(before, "\n")[bodyY]), ansi.StringWidth(after))
	result, payload = m.HandleClickType(right-2, bodyY)
	assert.Equal(t, ClickEditQueuedMessage, result)
	assert.Equal(t, "canonical-turn", payload)
	result, _ = m.HandleClickType(right, bodyY+1)
	assert.Equal(t, ClickQueuedMessage, result, "continuation has no action cluster")
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
	assert.True(t, strings.HasSuffix(ansi.Strip(line), directoryCopyIcon+"  "+directoryIcon+" "), "two icon separator cells and one whole-line trailing cell")
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

	require.Equal(t, 1, directoryIconWidth())
	var baseName string
	for _, progress := range []float64{0, .5, 1, .5, 0} {
		m.hoverValues = map[string]hoverValue{"directory": {value: progress, target: progress}, "directory-open": {value: progress, target: progress}}
		line := m.directoryRow(width)
		cells := sidebarCells(line)
		require.Len(t, cells, width)
		arrow := cells[width-2]
		assert.Equal(t, directoryIcon, arrow.glyph)
		expected := sidebarCells(directoryActionIcon(directoryIcon, progress, progress))
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
	copyX := arrowX - 3
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
			arrowBase = styles.TextPrimary
		} else {
			copyBase = styles.TextPrimary
		}
		assert.Equal(t, color.NRGBAModel.Convert(copyBase), color.NRGBAModel.Convert(cells[width-5].fg))
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
	for _, x := range []int{width - 5, width - 2} {
		assert.Equal(t, color.NRGBAModel.Convert(styles.Background), color.NRGBAModel.Convert(cells[x].fg), "all icons fade out with the row")
	}
}

func TestQueueActionsNarrowGeometryAndConfirmationLifetime(t *testing.T) {
	for _, width := range []int{1, 2, 3, 5, 7, 8, 10, 20, 80} {
		m := newHoverSidebar(t)
		m.SetSize(width, 50)
		settleTreePresentation(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "queued", Text: "界é 👩‍💻 first\ncontinuation"}}))
		w := m.contentWidth(m.cachedNeedsScrollbar)
		indent, actions := rowActions(w, false)
		for _, row := range m.placement.rows {
			if row.payload != "queued" || row.action == ClickNone {
				continue
			}
			require.LessOrEqual(t, ansi.StringWidth(row.text), w)
			for col := 0; col < w; col++ {
				if m.layoutCfg.PaddingLeft+col >= m.width-m.layoutCfg.PaddingRight {
					continue
				}
				result, _ := m.HandleClickType(m.layoutCfg.PaddingLeft+col, int(row.y)-m.scrollview.ScrollOffset())
				switch actions.PartAt(col-indent, row.queueControls) {
				case "edit":
					require.Equal(t, ClickEditQueuedMessage, result)
					require.Equal(t, "✎", ansi.Strip(ansi.Cut(row.text, col, col+1)))
				case "remove":
					require.Equal(t, ClickRemoveQueuedMessage, result)
					require.Equal(t, "×", ansi.Strip(ansi.Cut(row.text, col, col+1)))
				default:
					require.Equal(t, ClickQueuedMessage, result)
				}
			}
		}
		require.False(t, m.ConfirmQueuedRemoval("missing"))
		require.Empty(t, m.queueRemoveArmed)
		require.False(t, m.ConfirmQueuedRemoval("queued"))
		settleSidebarHover(t, m, m.ClearSubagentHover())
		require.Empty(t, m.queueRemoveArmed)
		require.False(t, m.ConfirmQueuedRemoval("queued"))
		m.SetQueuedMessages([]QueuedMessage{{ID: "queued", Text: "replacement"}})
		require.Empty(t, m.queueRemoveArmed)
		require.False(t, m.ConfirmQueuedRemoval("queued"))
		m.SetPresentationActive(false)
		require.Empty(t, m.queueRemoveArmed)
		require.Zero(t, m.ar.ActiveCount())
	}
}

func TestQueueActionHoverUsesSharedAnimationAndStableRows(t *testing.T) {
	m := newHoverSidebar(t)
	settleTreePresentation(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "queued", Text: "Unicode 界é 👩‍💻\ncontinuation"}}))
	row := requirePlaced(t, m, "queue:queued:0")
	width := m.contentWidth(m.cachedNeedsScrollbar)
	y := int(row.y) - m.scrollview.ScrollOffset()
	for _, part := range []struct {
		col  int
		name string
	}{{2, "text"}, {width - 3, "edit"}, {width - 1, "remove"}} {
		_, renders := m.CacheStats()
		cmd := m.updateRegionHover(m.layoutCfg.PaddingLeft+part.col, y)
		require.Equal(t, "queue:queued:"+part.name, m.hoverTarget)
		settleSidebarHover(t, m, cmd)
		require.Equal(t, 1.0, m.hoverValues[m.hoverTarget].value)
		require.Equal(t, row.text, requirePlaced(t, m, row.id).text, "hover never rewraps semantic queue rows")
		_, after := m.CacheStats()
		require.Equal(t, renders, after)
		require.Zero(t, m.ar.ActiveCount())
	}
	m.updateRegionHover(m.layoutCfg.PaddingLeft+2, y)
	m.SetPresentationActive(false)
	require.Empty(t, m.hoverValues)
	require.Zero(t, m.ar.ActiveCount(), "hidden page releases cosmetic leases")
}
