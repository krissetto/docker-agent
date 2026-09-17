package sidebar

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type placementClock struct {
	now  time.Time
	step time.Duration
}

func (c *placementClock) Now() time.Time { return c.now }
func (c *placementClock) Tick(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(c.step)
		return f(c.now)
	}
}

func newPlacementSidebar(t *testing.T, collapsed bool) *model {
	t.Helper()
	clock := &placementClock{now: time.Unix(1, 0), step: 50 * time.Millisecond}
	state := &service.SessionState{}
	state.SetCurrentAgentName("root")
	state.SetYoloMode(true)
	m := New(animation.NewRuntimeWithScheduler(clock), t.Context(), state).(*model)
	t.Cleanup(m.StopAnimation)
	m.rootSessionID = "placement"
	m.treeCollapsed = collapsed
	m.workingDirectory, m.gitBranchName = "placement-dir", ""
	require.Nil(t, m.SetAgentInfo("root", "provider/model-name", "", 1000, "", 0))
	require.Nil(t, m.SetSubagentTree(subagent.Snapshot{
		Root: "root:placement",
		Nodes: []subagent.NodeSnapshot{{
			Node: subagent.Node{ID: "root:placement", Agent: "root", State: subagent.NodeIdle},
			Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "turn-a", Agent: "worker", State: subagent.NodeIdle}},
				{Node: subagent.Node{ID: "turn-b", Agent: "worker", State: subagent.NodeIdle}},
			},
		}},
	}))
	require.Nil(t, m.SetSize(64, 30), "first preparation has no entrance animation")
	require.Nil(t, m.ReconcileLayout())
	require.NotNil(t, m.placement)
	require.Zero(t, m.ar.ActiveCount())
	return m
}

// Execute the owner's original Continue command, including Bubble Tea batches.
func placementTickMessage(t *testing.T, cmd tea.Cmd) animation.TickMsg {
	t.Helper()
	require.NotNil(t, cmd, "the production tick lease must not be lost")
	var ticks []animation.TickMsg
	var collect func(tea.Cmd)
	collect = func(next tea.Cmd) {
		if next == nil {
			return
		}
		switch msg := next().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				collect(child)
			}
		case animation.TickMsg:
			ticks = append(ticks, msg)
		default:
			t.Fatalf("unexpected presentation command message %T", msg)
		}
	}
	collect(cmd)
	require.Len(t, ticks, 1, "all sidebar sections share one timer")
	return ticks[0]
}

func advancePlacement(t *testing.T, m *model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	tick, ok := m.ar.Accept(placementTickMessage(t, sidebarOwnerCommand(m, cmd)))
	require.True(t, ok, "original tick lease remains valid across retargets")
	_, updateCmd := m.Update(tick)
	require.Nil(t, updateCmd, "fanout must not create another timer")
	return m.ar.Continue()
}

func settlePlacement(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	for range 20 {
		if !m.ar.HasActive() {
			require.Nil(t, cmd)
			assert.False(t, m.placement.running)
			return
		}
		cmd = advancePlacement(t, m, cmd)
	}
	t.Fatal("sidebar did not release its presentation/hover subscriptions")
}

func requirePlaced(t *testing.T, m *model, id string) placedRow {
	t.Helper()
	row, ok := findPlaced(m.placement.rows, id)
	require.True(t, ok, "missing canonical row %q", id)
	return row
}

func assertPlacementPaintAndHits(t *testing.T, m *model) string {
	t.Helper()
	view := m.View()
	lines := strings.Split(view, "\n")
	require.Len(t, lines, m.height)
	x := m.layoutCfg.PaddingLeft
	width := m.contentWidth(m.cachedNeedsScrollbar)
	for y := range m.viewportHeight() {
		painted, exists := m.paintedRows()[y+m.scrollview.ScrollOffset()]
		row, hit := m.placementRowAt(x, y)
		assert.Equal(t, exists && painted.target && painted.alpha > 0, hit, "row %d", y)
		if exists {
			assert.Equal(t, strings.TrimSpace(ansi.Strip(painted.text)), strings.TrimSpace(ansi.Strip(ansi.Cut(lines[y], x, x+width))), "painted row %d", y)
		}
		if hit {
			assert.Equal(t, painted.id, row.id)
			if row.action == ClickSubagent || row.action == ClickModel {
				action, payload := m.HandleClickType(x, y)
				assert.Equal(t, row.action, action)
				assert.Equal(t, row.payload, payload)
			}
		} else {
			action, _ := m.HandleClickType(x, y)
			assert.Equal(t, ClickNone, action)
		}
	}
	footer := lines[m.height-1]
	assert.Contains(t, ansi.Strip(footer), "YOLO")
	assert.Equal(t, strings.TrimSpace(ansi.Strip(m.footerView(m.contentWidth(false)))), strings.TrimSpace(ansi.Strip(footer)))
	assert.Equal(t, strings.Repeat(" ", m.layoutCfg.PaddingLeft)+m.footerView(m.contentWidth(false))+strings.Repeat(" ", m.layoutCfg.PaddingRight), footer, "pill is pinned to the content's right edge")
	for col := range m.width {
		action, _ := m.HandleClickType(col, m.height-1)
		assert.Equal(t, ClickNone, action, "agent identity and footer pills are passive")
	}
	return footer
}

func TestPlacementQueueCanonicalArrivalDrainAndRetarget(t *testing.T) {
	t.Parallel()
	for _, collapsed := range []bool{false, true} {
		t.Run(fmt.Sprintf("tree-collapsed=%t", collapsed), func(t *testing.T) {
			m := newPlacementSidebar(t, collapsed)
			initialModel := requirePlaced(t, m, "tree-summary")
			queue := []QueuedMessage{{ID: "queue-a", Text: "same text"}, {ID: "queue-b", Text: "same text"}}
			cmd := sidebarOwnerCommand(m, m.SetQueuedMessages(queue))
			require.NotNil(t, cmd)
			require.EqualValues(t, 1, m.ar.ActiveCount())
			a, b := requirePlaced(t, m, "queue:queue-a:0"), requirePlaced(t, m, "queue:queue-b:0")
			assert.NotEqual(t, a.id, b.id)
			assert.Equal(t, "- same text", strings.TrimSpace(ansi.Strip(a.text)), "queue previews use standalone hyphens, not agent connectors")
			assert.Equal(t, "- same text", strings.TrimSpace(ansi.Strip(b.text)))
			assert.InDelta(t, a.targetY+1, b.targetY, 0)
			assert.Zero(t, a.alpha)
			modelRow := requirePlaced(t, m, "tree-summary")
			assert.InDelta(t, initialModel.y, modelRow.y, 0)
			assert.Greater(t, modelRow.targetY, initialModel.y, "queue repositions the scrolling tree while model remains pinned")
			assert.Greater(t, modelRow.targetY, b.targetY+1, "queue has breathing space below")
			assert.Greater(t, a.targetY, float64(m.usageSectionEnd), "queue has breathing space above")

			cmd = advancePlacement(t, m, cmd)
			moving := requirePlaced(t, m, "tree-summary")
			want := initialModel.y + (modelRow.targetY-initialModel.y)*animation.EaseOutCubic(float64(50*time.Millisecond)/float64(350*time.Millisecond))
			assert.InDelta(t, want, moving.y, 1e-9)
			assert.Nil(t, m.SetQueuedMessages(queue), "identical queue does not rearm")
			beforeB := requirePlaced(t, m, "queue:queue-b:0")
			assert.Nil(t, m.SetQueuedMessages(queue[1:]), "drain reuses the pending shared tick")
			a = requirePlaced(t, m, "queue:queue-a:0")
			assert.False(t, a.target)
			assert.Equal(t, ClickNone, a.action)
			assert.Empty(t, a.controls)
			assert.InDelta(t, beforeB.y, requirePlaced(t, m, "queue:queue-b:0").fromY, 0)
			assert.InDelta(t, moving.y, requirePlaced(t, m, "tree-summary").fromY, 0)
			cmd = advancePlacement(t, m, cmd)
			beforeB = requirePlaced(t, m, "queue:queue-b:0")
			assert.Nil(t, m.SetQueuedMessages(queue), "rapid reversal does not replace the timer")
			assert.InDelta(t, beforeB.y, requirePlaced(t, m, "queue:queue-b:0").fromY, 0)
			settleStart := m.ar.Now()
			settlePlacement(t, m, cmd)
			assert.Equal(t, 350*time.Millisecond, m.ar.Now()-settleStart, "retarget uses the shared 350ms duration")
			ids, targets := map[string]bool{}, map[float64]bool{}
			for _, row := range m.placement.rows {
				if row.target {
					assert.False(t, ids[row.id], "duplicate target identity %q", row.id)
					assert.False(t, targets[row.targetY], "overlapping target rows")
					ids[row.id], targets[row.targetY] = true, true
				}
			}
			assertPlacementPaintAndHits(t, m)
			assert.Equal(t, 2, strings.Count(ansi.Strip(m.View()), "same text"))
			settlePlacement(t, m, m.SetQueuedMessages(nil))
			assert.NotContains(t, ansi.Strip(m.View()), "same text")
			assert.InDelta(t, initialModel.y, requirePlaced(t, m, "tree-summary").y, 0)
			assert.Zero(t, m.ar.ActiveCount())
		})
	}
}

func TestPlacementResizeScrollAndMovingCanonicalHover(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	queue := make([]QueuedMessage, 12)
	for i := range queue {
		queue[i] = QueuedMessage{ID: fmt.Sprintf("q-%d", i), Text: fmt.Sprintf("queued-%d", i)}
	}
	cmd := sidebarOwnerCommand(m, m.SetQueuedMessages(queue))
	cmd = advancePlacement(t, m, cmd)
	before := requirePlaced(t, m, "node:turn-b")
	assert.Nil(t, m.SetSize(32, 10))
	assert.InDelta(t, before.y, requirePlaced(t, m, "node:turn-b").fromY, 0, "resize starts at the current placement")
	footer := assertPlacementPaintAndHits(t, m)
	cmd = advancePlacement(t, m, cmd)
	m.scrollview.SetScrollOffset(max(0, int(math.Round(requirePlaced(t, m, "node:turn-b").y))-3))
	assert.Equal(t, footer, assertPlacementPaintAndHits(t, m))
	var hoverRow placedRow
	var hoverY int
	for y := range m.viewportHeight() {
		row, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, y)
		if ok && row.action == ClickSubagent {
			hoverRow, hoverY = row, y
			break
		}
	}
	require.NotEmpty(t, hoverRow.id, "fixture exposes a moving canonical node")
	_, hoverCmd := m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: hoverY})
	assert.Nil(t, hoverCmd, "hover joins the original placement clock")
	assert.Equal(t, hoverRow.id, m.hoverTarget)
	assert.Equal(t, subagent.NodeID(hoverRow.payload), m.hoveredSubagent)
	assert.Equal(t, footer, assertPlacementPaintAndHits(t, m))
	settlePlacement(t, m, cmd)
	assert.Equal(t, footer, assertPlacementPaintAndHits(t, m))
	settlePlacement(t, m, m.ClearSubagentHover())
	assert.Zero(t, m.ar.ActiveCount())
}

func TestPlacementUsageWrapAndColorOnlyUpdatesDoNotRetarget(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "placement", AgentContext: runtime.AgentContext{AgentName: "root"}, Usage: &runtime.Usage{ContextLength: 900000, ContextLimit: 1000000, Cost: 12345}})
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "other", AgentContext: runtime.AgentContext{AgentName: "root"}, Usage: &runtime.Usage{Cost: 42}})
	settlePlacement(t, m, m.ReconcileLayout())
	before := requirePlaced(t, m, "tree-summary")
	usageHeight := m.usageSectionEnd - m.usageReadingLine
	cmd := sidebarOwnerCommand(m, m.SetSize(24, 30))
	require.NotNil(t, cmd)
	assert.Greater(t, m.usageSectionEnd-m.usageReadingLine, usageHeight, "actual usage wrapping reflows subsequent sections")
	assert.Greater(t, requirePlaced(t, m, "tree-summary").targetY, before.targetY)
	cmd = advancePlacement(t, m, cmd)
	elapsed := m.placement.elapsed
	rows := slices.Clone(m.placement.rows)
	m.invalidateAnimation()
	assert.Nil(t, m.ReconcileLayout(), "content-only refresh does not restart layout")
	assert.Equal(t, elapsed, m.placement.elapsed)
	for _, old := range rows {
		row := requirePlaced(t, m, old.id)
		assert.InDelta(t, old.fromY, row.fromY, 0)
		assert.InDelta(t, old.targetY, row.targetY, 0)
	}
	settlePlacement(t, m, cmd)
	row := requirePlaced(t, m, "tree-summary")
	motion := tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: int(row.y)}
	_, cmd = m.Update(motion)
	settlePlacement(t, m, cmd)
	generation := m.VisualGeneration()
	rows = slices.Clone(m.placement.rows)
	for range 100 {
		_, cmd = m.Update(motion)
		assert.Nil(t, cmd)
		assert.Nil(t, m.ReconcileLayout())
	}
	assert.Equal(t, generation, m.VisualGeneration())
	assert.Equal(t, rows, m.placement.rows)
	assert.Zero(t, m.ar.ActiveCount())
	assert.Nil(t, m.ar.Continue())
}

func TestPlacementCancelRejectsOriginalPendingTick(t *testing.T) {
	t.Parallel()
	m := newPlacementSidebar(t, false)
	cmd := m.SetQueuedMessages([]QueuedMessage{{ID: "cancelled", Text: "cancel me"}})
	cmd = advancePlacement(t, m, cmd)
	assert.Nil(t, m.SetQueuedMessages(nil))
	m.CancelPresentation()
	assert.Zero(t, m.ar.ActiveCount())
	assert.False(t, m.placement.running)
	for _, row := range m.placement.rows {
		assert.InDelta(t, row.targetY, row.y, 0)
		assert.InDelta(t, row.targetAlpha, row.alpha, 0)
	}
	_, accepted := m.ar.Accept(placementTickMessage(t, cmd))
	assert.False(t, accepted, "hidden page cannot revive a cancelled lease")
	generation := m.VisualGeneration()
	m.CancelPresentation()
	assert.Equal(t, generation, m.VisualGeneration())
	assert.Nil(t, m.ar.Continue())
	assert.NotContains(t, ansi.Strip(m.View()), "cancel me")
}

func TestPlacementThemeRefreshAndViewDoNotChangeGeometry(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := newPlacementSidebar(t, false)
	cmd := m.SetQueuedMessages([]QueuedMessage{{ID: "themed", Text: "themed queue"}})
	cmd = advancePlacement(t, m, cmd)
	before := requirePlaced(t, m, "queue:themed:0")
	elapsed := m.placement.elapsed
	beforeView := m.View()
	beforeGeneration := m.visualGeneration
	beforeRows := slices.Clone(m.placement.rows)
	beforeOffset := m.scrollview.ScrollOffset()
	theme := *original
	theme.Colors.TextPrimary = "#12ab34"
	theme.Colors.TextBright = "#34ab12"
	styles.ApplyTheme(&theme)
	assert.NotEqual(t, beforeView, m.View(), "hot theme colors refresh even before a component update")
	assert.Equal(t, beforeRows, m.placement.rows, "hot-theme painting cannot reconcile live placements")
	assert.Equal(t, beforeGeneration, m.visualGeneration)
	assert.Equal(t, beforeOffset, m.scrollview.ScrollOffset())
	assert.Equal(t, elapsed, m.placement.elapsed)
	_, themeCmd := m.Update(messages.ThemeChangedMsg{})
	assert.Nil(t, themeCmd)
	after := requirePlaced(t, m, "queue:themed:0")
	assert.NotEqual(t, before.text, after.text, "cached cells must receive fresh theme colors")
	assert.InDelta(t, before.y, after.y, 0)
	assert.InDelta(t, before.fromY, after.fromY, 0)
	assert.InDelta(t, before.targetY, after.targetY, 0)
	assert.InDelta(t, before.alpha, after.alpha, 0)
	assert.Equal(t, elapsed, m.placement.elapsed)
	rows := slices.Clone(m.placement.rows)
	generation := m.VisualGeneration()
	cacheDirty, layoutDirty, reconcileDirty := m.cacheDirty, m.layoutDirty, m.reconcileDirty
	for range 100 {
		m.View()
	}
	assert.Equal(t, rows, m.placement.rows)
	assert.Equal(t, elapsed, m.placement.elapsed)
	assert.Equal(t, generation, m.VisualGeneration())
	assert.Equal(t, cacheDirty, m.cacheDirty)
	assert.Equal(t, layoutDirty, m.layoutDirty)
	assert.Equal(t, reconcileDirty, m.reconcileDirty)
	assert.EqualValues(t, 1, m.ar.ActiveCount())
	settlePlacement(t, m, cmd)
}
