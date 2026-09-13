package sidebar

import (
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

type branchSpans struct {
	count                     int
	from, value, target       float64
	elapsed                   time.Duration
	running                   bool
	idFrom, idValue, idTarget float64
	idElapsed                 time.Duration
	idRunning                 bool
}

type branchCapture struct {
	id        subagent.NodeID
	parent    subagent.NodeID
	sessionID string
	x, y      int
}

func (m *model) syncBranchSpans() tea.Cmd {
	if m.branchSpans == nil {
		m.branchSpans = make(map[subagent.NodeID]branchSpans)
	}
	seen := make(map[subagent.NodeID]bool)
	changed := false
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			id := node.Node.ID
			seen[id] = true
			span, exists := m.branchSpans[id]
			span.count = descendantCount(node.Children)
			target := 0.0
			if span.count > 0 && m.collapsedBranches[id] {
				target = 1
			}
			idTarget := 0.0
			if m.hoverTarget == "node:"+string(id) && m.hoveredSubagent == id {
				idTarget = 1
			}
			if !exists || !m.presentationActive {
				span.value, span.target = target, target
				span.running = false
				span.idValue, span.idTarget = idTarget, idTarget
				span.idRunning = false
			} else {
				if target != span.target {
					span.from, span.target, span.elapsed, span.running = span.value, target, 0, true
					changed = true
				}
				if idTarget != span.idTarget {
					span.idFrom, span.idTarget, span.idElapsed, span.idRunning = span.idValue, idTarget, 0, true
					changed = true
				}
			}
			m.branchSpans[id] = span
			walk(node.Children)
		}
	}
	walk(m.subagentNodes)
	for id := range m.branchSpans {
		if !seen[id] {
			delete(m.branchSpans, id)
		}
	}
	if changed {
		return m.presentationSub.Start()
	}
	return nil
}

func (m *model) branchSpansRunning() bool {
	for _, span := range m.branchSpans {
		if span.running || span.idRunning {
			return true
		}
	}
	return false
}

func (m *model) tickBranchSpans(delta time.Duration) bool {
	changed := false
	for id, span := range m.branchSpans {
		if span.running {
			span.elapsed += delta
			p := min(1, float64(span.elapsed)/float64(animation.ShortDuration))
			span.value = span.from + (span.target-span.from)*animation.EaseOutCubic(p)
			if p >= 1 {
				span.running = false
			}
			changed = true
		}
		if span.idRunning {
			span.idElapsed += delta
			p := min(1, float64(span.idElapsed)/float64(animation.ShortDuration))
			span.idValue = span.idFrom + (span.idTarget-span.idFrom)*animation.EaseOutCubic(p)
			if p >= 1 {
				span.idRunning = false
			}
			changed = true
		}
		m.branchSpans[id] = span
	}
	return changed
}

func spanWidth(text string, progress float64) int {
	return int(math.Round(float64(ansi.StringWidth(text)) * progress))
}

func neutralSpan(text string, progress float64, width int) string {
	if progress <= 0 || width <= 0 {
		return ""
	}
	return ansi.Cut(styles.FadeLine(styles.MutedStyle.Render(text), progress), 0, width)
}

func (m *model) capturedBranchAt(x, y int) (treeControl, bool) {
	capture := m.branchCapture
	if capture.id == "" || capture.x != x || capture.y != y {
		return treeControl{}, false
	}
	node, found := subagentview.Find(m.subagentNodes, capture.id)
	if !found || len(node.Children) == 0 || node.Node.Parent != capture.parent || node.Node.SessionID != capture.sessionID {
		return treeControl{}, false
	}
	row, ok := m.placementRowAt(m.layoutCfg.PaddingLeft, y)
	if !ok || row.id != "node:"+string(capture.id) {
		return treeControl{}, false
	}
	return treeControl{id: capture.id, x: x - m.layoutCfg.PaddingLeft}, true
}
