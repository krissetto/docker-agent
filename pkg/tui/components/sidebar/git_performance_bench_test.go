package sidebar

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func largeTreePerfFixture(n int) *model {
	m := New(animation.NewRuntime(), nil, &service.SessionState{}).(*model)
	m.treeCollapsed = false
	m.SetMode(ModeVertical)
	m.SetSize(40, 24)
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:perf", Agent: "root", State: subagent.NodeIdle}}
	for i := range n {
		root.Children = append(root.Children, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("node-%04d", i)), Agent: "worker", State: subagent.NodeIdle}})
	}
	m.rootSessionID = "perf"
	m.SetSubagentTree(subagent.Snapshot{Root: root.Node.ID, Nodes: []subagent.NodeSnapshot{root}})
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.View()
	return m
}

func BenchmarkSidebarLargeTreeHover(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			m := largeTreePerfFixture(n)
			rows := m.placement.rows
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				row := rows[i%len(rows)]
				_, _ = m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: int(row.targetY) % m.viewportHeight()})
				m.View()
			}
		})
	}
}

func BenchmarkSidebarLargeTreeScroll(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			m := largeTreePerfFixture(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				m.scrollview.SetScrollOffset(i % max(1, n-24))
				m.View()
			}
		})
	}
}

func largeGitPerfFixture(b *testing.B, n int) *model {
	b.Helper()
	ar := animation.NewRuntimeWithScheduler(&immediateScheduler{now: time.Unix(1, 0)})
	m := New(ar, b.Context(), &service.SessionState{}).(*model)
	m.SetMode(ModeVertical)
	m.SetSize(40, 24)
	files := make([]review.FileChange, n)
	for i := range files {
		files[i] = review.FileChange{Path: fmt.Sprintf("pkg/feature/file-%05d.go", i), Group: review.GroupStaged, Status: "M", Added: i%14 + 1, Removed: i % 8}
	}
	snapshot := &review.Snapshot{Repositories: []review.Repository{{ID: "repo", Name: "repo", Worktrees: []review.Worktree{{ID: "wt", Branch: "main", State: review.StateReady, CountsState: review.StateReady, Files: files}}}}}
	m.SetGitChanges(GitChanges{Available: true, Snapshot: snapshot})
	m.git.expanded = map[string]bool{gitSectionKey: true, "wt:wt": true, "grp:wt:2": true}
	m.git.rebuild()
	m.invalidateCache()
	m.ReconcileLayout()
	m.CancelPresentation()
	m.ReconcileLayout()
	m.View()
	return m
}

func BenchmarkSidebarLargeGitHover(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			m := largeGitPerfFixture(b, n)
			firstY := m.gitStart + 4
			secondY := firstY + 1
			if _, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, firstY); !ok {
				b.Fatal("first file row is not visible in fixture")
			}
			if _, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, secondY); !ok {
				b.Fatal("second file row is not visible in fixture")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				y := firstY
				if i%2 != 0 {
					y = secondY
				}
				m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: y})
				m.View()
			}
		})
	}
}

func BenchmarkSidebarLargeGitHoverOutside(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			m := largeGitPerfFixture(b, n)
			firstY := m.gitStart + 4
			if _, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, firstY); !ok {
				b.Fatal("file row is not visible in fixture")
			}
			m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: firstY})
			m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.Update(tea.MouseMotionMsg{X: -1, Y: -1})
				m.View()
				m.Update(tea.MouseMotionMsg{X: m.layoutCfg.PaddingLeft, Y: firstY})
				m.View()
			}
		})
	}
}

func BenchmarkSidebarLargeGitScroll(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			m := largeGitPerfFixture(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				m.scrollview.SetScrollOffset(i % max(1, n-24))
				m.View()
			}
		})
	}
}

func BenchmarkSidebarLargeGitAnimation(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			m := largeGitPerfFixture(b, n)
			m.workingAgent = "root"
			cmd := m.startSpinner()
			cmd = tea.Batch(cmd, m.ar.Continue())
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				msg := cmd().(animation.TickMsg)
				tick, ok := m.ar.Accept(msg)
				if !ok {
					b.Fatal("stale tick")
				}
				_, updateCmd := m.Update(tick)
				m.View()
				cmd = tea.Batch(updateCmd, m.ar.Continue())
			}
		})
	}
}
