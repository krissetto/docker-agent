package sidebar

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

const hugeTreeSessionID = "sidebar-huge-tree-perf"

type hugeTreeBenchCase struct {
	nodes int
	width int
}

var hugeTreeBenchCases = []hugeTreeBenchCase{
	{nodes: 1_000, width: 30},
	{nodes: 1_000, width: 60},
	{nodes: 5_000, width: 30},
	{nodes: 5_000, width: 60},
}

// hugeSessionTreeSnapshot models a session tree with up to four 10-way levels.
// The final level is partial for sizes that are not exact powers of ten.
func hugeSessionTreeSnapshot(n int, idleOnly bool) subagent.Snapshot {
	levelCounts := make([]int, 5)
	remaining, parents := n, 1
	for depth := 1; depth < len(levelCounts) && remaining > 0; depth++ {
		levelCounts[depth] = min(remaining, parents*10)
		remaining -= levelCounts[depth]
		parents = levelCounts[depth]
	}

	var build func(depth, index int, parentID subagent.NodeID) subagent.NodeSnapshot
	build = func(depth, index int, parentID subagent.NodeID) subagent.NodeSnapshot {
		id := subagent.NodeID(fmt.Sprintf("huge-%05d", indexAtDepth(depth, index)))
		nodeIndex := indexAtDepth(depth, index)
		state := subagent.NodeIdle
		if !idleOnly && nodeIndex%113 == 0 {
			state = subagent.NodeRunning
		} else if !idleOnly && nodeIndex%41 == 0 {
			state = subagent.NodeFailed
		}
		name := fmt.Sprintf("worker 界面 café long-session-agent-%05d", nodeIndex)
		node := subagent.Node{
			ID:             id,
			Agent:          fmt.Sprintf("agent-%d", nodeIndex%12),
			Name:           name,
			Description:    "Reviewing a long-running session tree with multilingual labels 界面 and café details",
			State:          state,
			NeedsAttention: nodeIndex%41 == 0,
			Parent:         parentID,
			SessionID:      fmt.Sprintf("%s-%05d", hugeTreeSessionID, nodeIndex),
			CreatedAt:      time.Unix(1_700_000_000+int64(nodeIndex), 0),
			UpdatedAt:      time.Unix(1_700_000_100+int64(nodeIndex), 0),
		}
		result := subagent.NodeSnapshot{Node: node}
		if depth+1 < len(levelCounts) {
			children := min(10, max(0, levelCounts[depth+1]-index*10))
			result.Children = make([]subagent.NodeSnapshot, children)
			for child := range children {
				result.Children[child] = build(depth+1, index*10+child, result.Node.ID)
			}
		}
		return result
	}

	rootID := subagent.SessionRootID(hugeTreeSessionID)
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: rootID, Agent: "root", State: subagent.NodeIdle}}
	root.Children = make([]subagent.NodeSnapshot, levelCounts[1])
	for i := range root.Children {
		root.Children[i] = build(1, i, rootID)
	}
	return subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{root}}
}

// indexAtDepth gives node IDs stable, unique ranges across tree levels.
func indexAtDepth(depth, index int) int {
	base := 0
	for level := 1; level < depth; level++ {
		base += pow10(level)
	}
	return base + index
}

func pow10(n int) int {
	result := 1
	for range n {
		result *= 10
	}
	return result
}

type hugeTreeBenchClock struct{ now time.Time }

func (c *hugeTreeBenchClock) Now() time.Time { return c.now }
func (c *hugeTreeBenchClock) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(100 * time.Millisecond)
		return create(c.now)
	}
}

func hugeSessionTreeSidebar(b *testing.B, snapshot subagent.Snapshot, width int) *model {
	return hugeSessionTreeSidebarWithRuntime(b, snapshot, width, animation.NewRuntime())
}

func hugeSessionTreeSidebarWithRuntime(b *testing.B, snapshot subagent.Snapshot, width int, ar *animation.Runtime) *model {
	b.Helper()
	m := New(ar, b.Context(), &service.SessionState{}).(*model)
	b.Cleanup(m.ar.Stop)
	m.rootSessionID = hugeTreeSessionID
	m.treeCollapsed = false
	m.SetMode(ModeVertical)
	m.SetSize(width, 24)
	m.SetSubagentTree(snapshot)
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.View()
	return m
}

func hugeTreeBenchName(c hugeTreeBenchCase) string {
	return fmt.Sprintf("nodes=%d/width=%d", c.nodes, c.width)
}

func setHugeTreeNodeRunning(nodes []subagent.NodeSnapshot, id subagent.NodeID) bool {
	for i := range nodes {
		if nodes[i].Node.ID == id {
			nodes[i].Node.State = subagent.NodeRunning
			return true
		}
		if setHugeTreeNodeRunning(nodes[i].Children, id) {
			return true
		}
	}
	return false
}

// BenchmarkHugeSessionTreeLoadAttach measures applying a loaded session
// snapshot and reconciling the first full sidebar tree layout on an empty view.
func BenchmarkHugeSessionTreeLoadAttach(b *testing.B) {
	for _, c := range hugeTreeBenchCases {
		b.Run(hugeTreeBenchName(c), func(b *testing.B) {
			snapshot := hugeSessionTreeSnapshot(c.nodes, false)
			m := hugeSessionTreeSidebar(b, snapshot, c.width)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				// Reuse the component (and its filesystem watcher) while restoring
				// the state that a new session's tree attachment starts from.
				m.subagentSpinner.Stop()
				m.subagentSpinnerOn = false
				m.subagentNodes = nil
				m.delegationRoot = nil
				m.placement = nil
				m.invalidateCache()
				m.SetSubagentTree(snapshot)
				m.ReconcileLayout()
				m.View()
			}
		})
	}
}

// BenchmarkHugeSessionTreeColdView measures a full two-pass render-cache rebuild
// after the tree has already been attached and its viewport is scrolled.
func BenchmarkHugeSessionTreeColdView(b *testing.B) {
	for _, c := range hugeTreeBenchCases {
		b.Run(hugeTreeBenchName(c), func(b *testing.B) {
			m := hugeSessionTreeSidebar(b, hugeSessionTreeSnapshot(c.nodes, false), c.width)
			m.scrollview.SetScrollOffset(max(0, m.placement.extent*2/3))
			m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				// ReconcileLayout rebuilds actual row geometry after a semantic
				// cache invalidation, then View paints the resulting viewport.
				m.invalidateCache()
				m.ReconcileLayout()
				m.View()
			}
		})
	}
}

// BenchmarkHugeSessionTreeSteadyView contrasts a cached idle frame with the
// animation-only invalidation/render path used while a subagent is running.
func BenchmarkHugeSessionTreeSteadyView(b *testing.B) {
	for _, c := range hugeTreeBenchCases {
		b.Run(hugeTreeBenchName(c)+"/idle", func(b *testing.B) {
			m := hugeSessionTreeSidebar(b, hugeSessionTreeSnapshot(c.nodes, true), c.width)
			m.scrollview.SetScrollOffset(max(0, m.placement.extent*2/3))
			m.View()
			if m.ar.ActiveCount() != 0 {
				b.Fatal("idle tree unexpectedly registered animation work")
			}
			invalidations, renders := m.CacheStats()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.View()
			}
			b.StopTimer()
			gotInvalidations, gotRenders := m.CacheStats()
			if gotInvalidations != invalidations || gotRenders != renders {
				b.Fatalf("cached idle view did work: invalidations %d->%d renders %d->%d", invalidations, gotInvalidations, renders, gotRenders)
			}
		})
		b.Run(hugeTreeBenchName(c)+"/active-animation-frame", func(b *testing.B) {
			snapshot := hugeSessionTreeSnapshot(c.nodes, false)
			scheduler := &hugeTreeBenchClock{now: time.Unix(1, 0)}
			m := hugeSessionTreeSidebarWithRuntime(b, snapshot, c.width, animation.NewRuntimeWithScheduler(scheduler))
			m.scrollview.SetScrollOffset(max(0, m.placement.extent*2/3))
			visibleID := ""
			offset := m.scrollview.ScrollOffset()
			for _, row := range m.placement.rows {
				if row.action == ClickSubagent && int(row.targetY) >= offset && int(row.targetY) < offset+m.viewportHeight() {
					visibleID = row.payload
					break
				}
			}
			if visibleID == "" {
				b.Fatal("fixture has no visible tree rows")
			}
			if !setHugeTreeNodeRunning(snapshot.Nodes, subagent.NodeID(visibleID)) {
				b.Fatalf("visible node %q not found in snapshot", visibleID)
			}
			m.SetSubagentTree(snapshot)
			m.ReconcileLayout()
			m.View()
			if m.ar.ActiveCount() == 0 {
				b.Fatal("active session tree should register spinner animation")
			}
			m.View()
			prepared := m.preparedTrees[m.placement.width]
			line := prepared.rowByID[subagent.NodeID(visibleID)]
			beforeFrame := prepared.rows[line].frame
			_, beforeRenders := m.CacheStats()
			cmd := m.ar.Continue()
			if cmd == nil {
				b.Fatal("active session tree should schedule spinner ticks")
			}
			for range 12 {
				if cmd == nil {
					b.Fatal("active animation stopped scheduling ticks before frame changed")
				}
				tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
				if !ok {
					b.Fatal("stale animation tick")
				}
				m.Update(tick)
				m.View()
				if prepared.rows[line].frame != beforeFrame {
					break
				}
				cmd = m.ar.Continue()
			}
			if prepared.rows[line].frame == beforeFrame {
				b.Fatal("accepted active ticks did not change visible spinner row")
			}
			_, afterRenders := m.CacheStats()
			if afterRenders != beforeRenders+1 {
				b.Fatalf("tree-only animation should refresh only the affected tree subsection: renders %d->%d", beforeRenders, afterRenders)
			}
			cmd = m.ar.Continue()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if cmd == nil {
					b.Fatal("active animation stopped scheduling ticks")
				}
				tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
				if !ok {
					b.Fatal("stale animation tick")
				}
				m.Update(tick)
				m.View()
				cmd = m.ar.Continue()
			}
		})
	}
}

// BenchmarkHugeSessionTreeHoverMotion moves across visible tree rows at a
// scrolled offset, exercising per-row hover presentation and pointer lookup.
func BenchmarkHugeSessionTreeHoverMotion(b *testing.B) {
	for _, c := range hugeTreeBenchCases {
		b.Run(hugeTreeBenchName(c), func(b *testing.B) {
			m := hugeSessionTreeSidebar(b, hugeSessionTreeSnapshot(c.nodes, false), c.width)
			offset := max(0, m.placement.extent*2/3)
			m.scrollview.SetScrollOffset(offset)
			m.View()
			rows := make([]placedRow, 0, m.viewportHeight())
			for _, row := range m.placement.rows {
				if row.action == ClickSubagent && int(row.targetY) >= offset && int(row.targetY) < offset+m.viewportHeight() {
					rows = append(rows, row)
				}
			}
			if len(rows) < 2 {
				b.Fatal("fixture has too few visible subagent rows for hover")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				row := rows[i%len(rows)]
				_, _ = m.Update(tea.MouseMotionMsg{
					X: m.layoutCfg.PaddingLeft + 4,
					Y: int(row.targetY) - offset,
				})
				m.View()
			}
		})
	}
}

// BenchmarkHugeSessionTreeScroll measures cached viewport composition while
// cycling through the scrolled tree; it intentionally does not rebuild rows.
func BenchmarkHugeSessionTreeScroll(b *testing.B) {
	for _, c := range hugeTreeBenchCases {
		b.Run(hugeTreeBenchName(c), func(b *testing.B) {
			m := hugeSessionTreeSidebar(b, hugeSessionTreeSnapshot(c.nodes, false), c.width)
			maxOffset := max(1, m.placement.extent-m.viewportHeight())
			m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				m.scrollview.SetScrollOffset(i % maxOffset)
				m.View()
			}
		})
	}
}
