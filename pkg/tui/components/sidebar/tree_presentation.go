package sidebar

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/subagent"
)

type counterPresentation struct {
	initialized         bool
	value               int
	alpha, from, target float64
	elapsed             time.Duration
	running             bool
}

type preparedTree struct {
	lines     []string
	bodyStart int
	nodes     []subagent.NodeID
	controls  map[int][]treeControl
	agents    map[int]string
}

func (m *model) syncCounters() tea.Cmd {
	_, active, attention := m.treeCounts()
	changed := false
	for i, value := range []int{active, attention} {
		counter := &m.treeCounters[i]
		target := 0.0
		if value > 0 {
			target = 1
		}
		if !counter.initialized || !m.presentationActive {
			counter.initialized = true
			counter.value = value
			counter.alpha = target
			counter.target = target
			counter.running = false
			continue
		}
		if value > 0 {
			counter.value = value
		}
		if target == counter.target {
			continue
		}
		counter.from, counter.target = counter.alpha, target
		counter.elapsed = 0
		counter.running = true
		changed = true
	}
	if changed {
		return m.presentationSub.Start()
	}
	return nil
}

func (m *model) countersRunning() bool {
	for _, counter := range m.treeCounters {
		if counter.running {
			return true
		}
	}
	return false
}

func (m *model) subagentsInfo(width int) string {
	if m.preparedTrees == nil {
		m.preparedTrees = make(map[int]preparedTree)
	}
	prepared, ok := m.preparedTrees[width]
	if !ok {
		text := m.prepareTreeInfo(width)
		prepared = preparedTree{bodyStart: m.subagentRowOffset, nodes: append([]subagent.NodeID(nil), m.subagentLineNodes...), controls: m.treeControls, agents: m.transferAgentLines}
		if text != "" {
			prepared.lines = strings.Split(text, "\n")
		}
		if !m.hasTreeContent() {
			prepared.bodyStart = len(prepared.lines)
		}
		m.preparedTrees[width] = prepared
	}
	m.treeControls = make(map[int][]treeControl)
	m.transferAgentLines = make(map[int]string)
	m.subagentLineNodes = nil
	m.subagentRowOffset = prepared.bodyStart
	m.delegationRootLine = -1
	if len(prepared.lines) == 0 {
		return ""
	}
	lines := append([]string(nil), prepared.lines...)
	if m.hasTreeContent() {
		summaryRow := 0
		if m.parentAgent != "" {
			summaryRow = 2
		}
		lines[summaryRow] = m.hoverText(m.treeSummary(width), "tree-summary")
	}
	bodyRows := max(0, len(lines)-prepared.bodyStart)
	visible := bodyRows
	if m.treeCollapsed {
		visible = 0
	}
	end := min(len(lines), prepared.bodyStart+visible)
	if m.treeCollapsed && m.hasTreeContent() {
		end = max(0, prepared.bodyStart-1)
	}
	lines = lines[:end]
	m.subagentLineNodes = append(m.subagentLineNodes, prepared.nodes[:min(len(prepared.nodes), visible)]...)
	for row, controls := range prepared.controls {
		if row < end {
			m.treeControls[row] = controls
		}
	}
	for row, name := range prepared.agents {
		if row < end {
			m.transferAgentLines[row] = name
		}
	}
	return strings.Join(lines, "\n")
}

// CancelPresentation releases only visual transitions when a page stops receiving ticks.
func (m *model) CancelPresentation() {
	changed := m.countersRunning() || m.branchSpansRunning()
	for id, span := range m.branchSpans {
		span.value = span.target
		span.running = false
		span.idValue = span.idTarget
		span.idRunning = false
		m.branchSpans[id] = span
	}
	for i := range m.treeCounters {
		m.treeCounters[i].running = false
		m.treeCounters[i].alpha = m.treeCounters[i].target
	}
	m.CancelHover()
	if m.placement != nil {
		changed = changed || m.placement.running
		m.placement.running = false
		for i := range m.placement.rows {
			row := &m.placement.rows[i]
			row.y = row.targetY
			row.alpha = row.targetAlpha
		}
	}
	m.presentationSub.Stop()
	if changed {
		m.cacheDirty, m.layoutDirty = true, true
		m.visualGeneration++
	}
}

// SetPresentationActive gates finite UI animation before hidden-page updates.
func (m *model) SetPresentationActive(active bool) tea.Cmd {
	if m.presentationActive == active {
		return nil
	}
	m.presentationActive = false
	m.CancelPresentation()
	m.reconcileDirty = true
	m.ReconcileLayout()
	m.presentationActive = active
	return nil
}
