package sidebar

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
)

type counterPresentation struct {
	initialized         bool
	value               int
	alpha, from, target float64
	elapsed             time.Duration
	running             bool
}

type preparedTreeRow struct {
	identity identityColumns
	node     subagent.Node
	guides   string
	branch   bool
	text     string
	controls []treeControl
	hover    hoverValue
	span     branchSpans
	hovered  bool
	frame    string
}

type preparedTree struct {
	rootCount int
	rows      map[int]preparedTreeRow
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
	if !ok || prepared.rootCount != len(m.subagentNodes) {
		m.treeCountsValid = false
		text := m.prepareTreeInfo(width)
		prepared = preparedTree{rootCount: len(m.subagentNodes), rows: m.preparingRows, bodyStart: m.subagentRowOffset, nodes: append([]subagent.NodeID(nil), m.subagentLineNodes...), controls: m.treeControls, agents: m.transferAgentLines}
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
	indent := min(2, max(0, width-1))
	if !m.treeCollapsed {
		for row, item := range prepared.rows {
			item = m.refreshPreparedTreeRow(item, row, width)
			prepared.rows[row] = item
			lines[row] = item.text
			if len(item.controls) > 0 {
				m.treeControls[row] = item.controls
			}
		}
		for row, name := range prepared.agents {
			lines[row] = strings.Repeat(" ", indent) + m.participantLine(name, max(1, width-indent))
		}
		if pres, ok := m.visibleTransfer(); ok && len(lines) > 0 {
			lines[len(lines)-1] = m.renderTransferRelation(pres, width)
		}
	}
	if m.parentAgent != "" {
		lines[0] = ansi.Truncate(m.parentLine(), width, "…")
	}
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
	if m.placement != nil {
		m.placement.painted = nil
	}
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
	m.StopAnimation()
	m.reconcileDirty = true
	m.ReconcileLayout()
	m.presentationActive = active
	if !active {
		return nil
	}
	var cmds []tea.Cmd
	if m.needsSpinner() {
		cmds = append(cmds, m.startSpinner())
	}
	cmds = append(cmds, m.syncSubagentSpinner())
	if _, visible := m.visibleTransfer(); visible {
		cmds = append(cmds, m.transferAnimation.Start())
	}
	for _, state := range m.ragIndexing {
		cmds = append(cmds, state.spinner.Init())
	}
	return tea.Batch(cmds...)
}

// Reuse immutable row styling until its own presentation channels change. The
// controls are replaced together with the text so pointer geometry stays exact.
func (m *model) refreshPreparedTreeRow(item preparedTreeRow, row, width int) preparedTreeRow {
	hover, span := m.hoverValues["node:"+string(item.node.ID)], m.branchSpans[item.node.ID]
	hovered := m.hoveredSubagent == item.node.ID
	frame := ""
	if isActiveSubagentState(item.node.State) {
		frame = m.subagentSpinner.RawFrame() + m.spinner.RawFrame()
	}
	if item.text != "" && item.hover == hover && item.span == span && item.hovered == hovered && item.frame == frame {
		return item
	}
	indent := min(2, max(0, width-1))
	delete(m.treeControls, row)
	text, identity := m.subagentRowIdentity(item.node, item.guides, max(1, width-indent), row, item.branch)
	item.text = strings.Repeat(" ", indent) + text
	item.identity = identity.shift(indent)
	for i := range m.treeControls[row] {
		m.treeControls[row][i].x += indent
	}
	item.controls = m.treeControls[row]
	item.hover, item.span, item.hovered, item.frame = hover, span, hovered, frame
	return item
}
