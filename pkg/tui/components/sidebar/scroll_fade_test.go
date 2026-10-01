package sidebar

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestPlacedSidebarReusesSharedViewportFadeAndKeepsFooter(t *testing.T) {
	t.Parallel()
	m := newHoverSidebar(t)
	m.SetPresentationActive(false)
	m.sessionState.SetYoloMode(true)
	snap := subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root"}}}}
	for i := range 30 {
		snap.Nodes[0].Children = append(snap.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("node-%d", i)), Agent: fmt.Sprintf("worker-%d", i), State: subagent.NodeIdle}})
	}
	m.SetSubagentTree(snap)
	m.SetQueuedMessages([]QueuedMessage{{ID: "queued", Text: "first queued row\nsecond queued row"}})
	m.SetSize(40, 12)
	m.ReconcileLayout()
	height := m.viewportHeight()
	total := m.placementExtent()
	for _, offset := range []int{0, 8, total - height} {
		m.scrollview.SetScrollOffset(offset)
		actual := strings.Split(m.View(), "\n")
		require.Len(t, actual, m.height)
		painted := m.paintedRows()
		rows := make([]string, height)
		for i := range rows {
			if row, ok := painted[offset+i]; ok {
				rows[i] = padRight(m.placementText(row, m.contentWidth(m.cachedNeedsScrollbar)), m.contentWidth(m.cachedNeedsScrollbar))
			}
		}
		reference := scrollview.New()
		reference.SetSize(m.width-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, height)
		reference.SetContent(make([]string, total), total)
		reference.SetScrollOffset(offset)
		expected := strings.Split(reference.ViewWithPaddedLines(rows), "\n")
		for i := range height {
			assertFadeCells(t, expected[i], strings.TrimPrefix(actual[i], strings.Repeat(" ", m.layoutCfg.PaddingLeft)))
		}
		assert.Equal(t, strings.Repeat(" ", m.layoutCfg.PaddingLeft)+m.footerView(m.contentWidth(false)), actual[m.height-1], "pinned pill remains byte-exact/unfaded")
		assert.Empty(t, strings.TrimSpace(ansi.Strip(actual[height])), "reserved footer breathing row stays blank/unfaded")
		before := m.View()
		generation := m.VisualGeneration()
		assert.Equal(t, before, m.View(), "warm frame stable")
		assert.Equal(t, generation, m.VisualGeneration(), "render cannot request another frame")
	}
	m.SetSubagentTree(subagent.Snapshot{Root: "root:hover", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:hover", Agent: "root"}}}})
	m.SetQueuedMessages(nil)
	m.ReconcileLayout()
	m.scrollview.SetScrollOffset(0)
	view := strings.Split(m.View(), "\n")
	assert.False(t, m.cachedNeedsScrollbar)
	require.NotEmpty(t, m.placement.rows)
	assertFadeCells(t, m.placement.rows[0].text, strings.TrimPrefix(view[0], strings.Repeat(" ", m.layoutCfg.PaddingLeft)))
	assert.Contains(t, ansi.Strip(view[len(view)-1]), "YOLO")
}

func assertFadeCells(t *testing.T, want, got string) {
	t.Helper()
	expected, actual := sidebarCells(want), sidebarCells(got)
	require.Len(t, actual, len(expected))
	for i, cell := range expected {
		assert.Equal(t, cell.glyph, actual[i].glyph, "glyph column%d", i)
		fg := cell.fg
		if fg == nil {
			fg = styles.TextPrimary
		}
		gotFg := actual[i].fg
		if gotFg == nil {
			gotFg = styles.TextPrimary
		}
		assert.Equal(t, color.NRGBAModel.Convert(fg), color.NRGBAModel.Convert(gotFg), "foreground column%d", i)
		bg, gotBg := cell.bg, actual[i].bg
		if bg == nil {
			bg = styles.Background
		}
		if gotBg == nil {
			gotBg = styles.Background
		}
		assert.Equal(t, color.NRGBAModel.Convert(bg), color.NRGBAModel.Convert(gotBg), "background column%d", i)
	}
}
