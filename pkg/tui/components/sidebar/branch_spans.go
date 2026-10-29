package sidebar

import (
	"math"
	"strings"
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
	var walk func([]subagent.NodeSnapshot) int
	walk = func(nodes []subagent.NodeSnapshot) int {
		total := 0
		for _, node := range nodes {
			id := node.Node.ID
			seen[id] = true
			span, exists := m.branchSpans[id]
			span.count = walk(node.Children)
			total += 1 + span.count
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
		}
		return total
	}
	walk(m.subagentNodes)
	for id := range m.branchSpans {
		if !seen[id] {
			delete(m.branchSpans, id)
		}
	}
	m.refreshBranchSpanIndex()
	if changed {
		return m.presentationSub.Start()
	}
	return nil
}

func (m *model) refreshBranchSpanIndex() {
	if m.placement == nil {
		return
	}
	m.placement.runningSpanIDs, m.placement.runningSpanSet = runningBranchSpanIndex(m.branchSpans)
	m.placement.spanIndexValid = true
}

func (m *model) setIndexedSpanRunning(id subagent.NodeID, running bool) {
	p := m.placement
	if p == nil || !p.spanIndexValid {
		return
	}
	if running {
		if p.runningSpanSet[id] {
			return
		}
		p.runningSpanSet[id] = true
		p.runningSpanIDs = append(p.runningSpanIDs, id)
		return
	}
	if !p.runningSpanSet[id] {
		return
	}
	delete(p.runningSpanSet, id)
	for i, candidate := range p.runningSpanIDs {
		if candidate == id {
			p.runningSpanIDs = append(p.runningSpanIDs[:i], p.runningSpanIDs[i+1:]...)
			break
		}
	}
}

func runningBranchSpanIndex(spans map[subagent.NodeID]branchSpans) ([]subagent.NodeID, map[subagent.NodeID]bool) {
	ids := make([]subagent.NodeID, 0)
	set := make(map[subagent.NodeID]bool)
	for id, span := range spans {
		if span.running || span.idRunning {
			ids = append(ids, id)
			set[id] = true
		}
	}
	return ids, set
}

func (m *model) branchHoverNodeID() subagent.NodeID {
	if !strings.HasPrefix(m.hoverTarget, "node:") {
		return ""
	}
	return subagent.NodeID(strings.TrimPrefix(m.hoverTarget, "node:"))
}

func (m *model) branchSpansRunning() bool {
	if m.placement != nil && m.placement.spanIndexValid {
		return len(m.placement.runningSpanIDs) > 0
	}
	for _, span := range m.branchSpans {
		if span.running || span.idRunning {
			return true
		}
	}
	return false
}

func (m *model) tickBranchSpans(delta time.Duration) bool {
	changed := false
	ids := m.branchSpanCandidates()
	keep := 0
	for _, id := range ids {
		span, exists := m.branchSpans[id]
		if !exists {
			continue
		}
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
		if m.placement != nil && m.placement.spanIndexValid {
			if span.running || span.idRunning {
				m.placement.runningSpanIDs[keep] = id
				keep++
			} else {
				delete(m.placement.runningSpanSet, id)
			}
		}
	}
	if m.placement != nil && m.placement.spanIndexValid {
		m.placement.runningSpanIDs = m.placement.runningSpanIDs[:keep]
	}
	return changed
}

func (m *model) branchSpanCandidates() []subagent.NodeID {
	if m.placement != nil && m.placement.spanIndexValid {
		return m.placement.runningSpanIDs
	}
	ids, _ := runningBranchSpanIndex(m.branchSpans)
	return ids
}

func spanWidth(text string, progress float64) int {
	return int(math.Round(float64(ansi.StringWidth(text)) * progress))
}

// neutralSpan reveals text by width only, always in the stable muted base color.
// Reveal progress never fades that color. Callers wrap the result in the row's
// shared pointer HoverText, which is the only pointer brightness path.
func neutralSpan(text string, progress float64, width int) string {
	if progress <= 0 || width <= 0 {
		return ""
	}
	return ansi.Cut(styles.MutedStyle.Render(text), 0, width)
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

// Hover changes only ID reveal targets, not branch topology or descendant counts.
func (m *model) syncBranchHover() tea.Cmd {
	current := m.branchHoverNodeID()
	ids := []subagent.NodeID{current}
	if m.placement != nil && m.placement.hoveredBranchID != "" {
		ids = append(ids, m.placement.hoveredBranchID)
	}
	changed := false
	for i, id := range ids {
		if id == "" || (i > 0 && id == ids[0]) {
			continue
		}
		span, exists := m.branchSpans[id]
		if !exists {
			continue
		}
		target := 0.0
		if m.hoverTarget == "node:"+string(id) && m.hoveredSubagent == id {
			target = 1
		}
		if target == span.idTarget {
			continue
		}
		span.idFrom, span.idTarget, span.idElapsed = span.idValue, target, 0
		span.idRunning = m.presentationActive
		if !m.presentationActive {
			span.idValue = target
		}
		m.branchSpans[id] = span
		m.setIndexedSpanRunning(id, span.running || span.idRunning)
		changed = true
	}
	if m.placement != nil {
		m.placement.hoveredBranchID = current
	}
	if changed && m.presentationActive {
		return m.presentationSub.Start()
	}
	return nil
}
