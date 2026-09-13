package sidebar

import (
	"fmt"
	"slices"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type treeControl struct {
	id    subagent.NodeID
	x     int
	whole bool
}

func (m *model) canonicalTreeID() subagent.NodeID {
	if m.delegationRoot != nil {
		return m.delegationRoot.ID
	}
	return m.treeRootID()
}

func (m *model) transferParticipants() []string {
	names := []string{m.delegationRootName()}
	add := func(name string) {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, hop := range m.agentTransfers {
		add(hop.from)
		add(hop.to)
	}
	if pres, ok := m.visibleTransfer(); ok {
		add(pres.from)
		add(pres.to)
	}
	return names[1:]
}

func (m *model) pruneCollapsedBranches() {
	ids := make(map[subagent.NodeID]bool)
	if id := m.canonicalTreeID(); id != "" {
		ids[id] = true
	}
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			ids[node.Node.ID] = true
			walk(node.Children)
		}
	}
	walk(m.subagentNodes)
	for id := range m.collapsedBranches {
		if !ids[id] {
			delete(m.collapsedBranches, id)
		}
	}
}

func treeControlReserve(width int, id subagent.NodeID, branch, root bool) int {
	if width < 4 {
		return 0
	}
	reserve := 0
	if branch && id != "" {
		reserve += 2
	}
	if root {
		reserve += 2
	}
	if reserve >= width {
		return 2
	}
	return reserve
}

// decorateTreeRow reserves independent control cells; identity clicks keep the rest.
func (m *model) decorateTreeRow(line string, row, width int, id subagent.NodeID, branch, root bool) string {
	if width < 4 {
		return line
	}
	reserve := treeControlReserve(width, id, branch, root)
	if root && reserve == 2 {
		branch = false
	}
	if reserve == 0 {
		return line
	}
	bodyWidth := max(1, width-reserve)
	line = padRight(ansi.Truncate(line, bodyWidth, "…"), bodyWidth)
	hovered := m.hoveredTreeRow == row
	if branch && id != "" {
		x := lipgloss.Width(line) + 1
		glyph := " "
		if hovered {
			glyph = "⌄"
			if m.collapsedBranches[id] {
				glyph = "›"
			}
		}
		line += " " + styles.MutedStyle.Render(glyph)
		m.treeControls[row] = append(m.treeControls[row], treeControl{id: id, x: x})
	}
	if root {
		x := lipgloss.Width(line) + 1
		glyph := " "
		if hovered {
			glyph = "−"
		}
		line += " " + styles.MutedStyle.Render(glyph)
		m.treeControls[row] = append(m.treeControls[row], treeControl{x: x, whole: true})
	}
	return line
}

func (m *model) treeSummary(width int) string {
	total, active, attention := 0, 0, 0
	add := func(n subagent.Node) {
		total++
		if isActiveSubagentState(n.State) {
			active++
		}
		if n.NeedsAttention {
			attention++
		}
	}
	if m.delegationRoot != nil {
		add(*m.delegationRoot)
	} else if name := m.delegationRootName(); name != "" {
		total++
		if m.workingAgent == name {
			active++
		}
	}
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			add(node.Node)
			walk(node.Children)
		}
	}
	walk(m.subagentNodes)
	for _, name := range m.participantNames() {
		total++
		if m.workingAgent == name {
			active++
		}
	}
	text := fmt.Sprintf("› %d total · %d active · %d attention", total, active, attention)
	if lipgloss.Width(text) > width {
		text = fmt.Sprintf("› %d total %d active %d!", total, active, attention)
	}
	if lipgloss.Width(text) > width {
		text = fmt.Sprintf("› %d/%d/%d!", total, active, attention)
	}
	if lipgloss.Width(text) > width {
		text = fmt.Sprintf("%d/%d/%d", total, active, attention)
	}
	return styles.MutedStyle.Render(ansi.Truncate(text, width, "…"))
}

func (m *model) treeControlAt(x, y int) (treeControl, bool) {
	if m.layoutDirty {
		m.View()
	}
	if m.mode != ModeVertical || y < 0 || y >= m.height || x < m.layoutCfg.PaddingLeft {
		return treeControl{}, false
	}
	width := m.contentWidth(m.cachedNeedsScrollbar)
	x -= m.layoutCfg.PaddingLeft
	if x >= width {
		return treeControl{}, false
	}
	row := y + m.scrollview.ScrollOffset() - m.treeSectionStart
	if m.treeCollapsed && row == 0 {
		return treeControl{whole: true}, true
	}
	if row != m.hoveredTreeRow {
		return treeControl{}, false
	}
	for _, control := range m.treeControls[row] {
		if x == control.x {
			return control, true
		}
	}
	return treeControl{}, false
}

func (m *model) toggleTreeControl(control treeControl) {
	if control.whole {
		m.treeCollapsed = !m.treeCollapsed
	} else {
		if m.collapsedBranches == nil {
			m.collapsedBranches = make(map[subagent.NodeID]bool)
		}
		m.collapsedBranches[control.id] = !m.collapsedBranches[control.id]
	}
	m.hoveredSubagent = ""
	m.hoveredTreeRow = -1
	m.invalidateCache()
}

func (m *model) updateTreeRowHover(y int) {
	row := y + m.scrollview.ScrollOffset() - m.treeSectionStart
	if _, ok := m.treeControls[row]; !ok {
		row = -1
	}
	if row != m.hoveredTreeRow {
		m.hoveredTreeRow = row
		m.invalidateAnimation()
	}
}

// Usage carries session identity even when the legacy background runner has no
// async node. Match represented sessions, never merge distinct nodes by name.
func (m *model) participantNames() []string {
	names := m.transferParticipants()
	represented := map[string]bool{m.rootSessionID: true}
	if m.delegationRoot != nil {
		represented[m.delegationRoot.SessionID] = true
	}
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			represented[node.Node.SessionID] = true
			walk(node.Children)
		}
	}
	walk(m.subagentNodes)
	var legacy []string
	for sessionID, name := range m.usageOwners {
		if represented[sessionID] || name == m.delegationRootName() || slices.Contains(names, name) || slices.Contains(legacy, name) {
			continue
		}
		if _, ok := m.sessionState.AgentUsage(name); ok {
			legacy = append(legacy, name)
		}
	}
	slices.Sort(legacy)
	return append(names, legacy...)
}

func (m *model) participantLine(name string, width int) string {
	reading := m.agentContextPercent(name)
	room := width - 2
	if reading != "" {
		room -= lipgloss.Width(reading) + 1
	}
	if room < 1 {
		reading = ""
		room = max(1, width-2)
	}
	line := styles.MutedStyle.Render("  ") + styles.AgentIdentityStyle(name, false).Render(ansi.Truncate(name, room, "…"))
	if reading != "" {
		line = padRight(line, width-lipgloss.Width(reading)) + contextGaugeStyle(m.agentContextGaugeLevel(name), styles.MutedStyle).Render(reading)
	}
	return ansi.Truncate(line, width, "")
}
