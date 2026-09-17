package sidebar

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type treeControl struct {
	id    subagent.NodeID
	x     int
	whole bool
	todos bool
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

func (m *model) treeCounts() (total, active, attention int) {
	if m.treeCountsValid {
		return m.treeCountCache[0], m.treeCountCache[1], m.treeCountCache[2]
	}
	defer func() { m.treeCountCache = [3]int{total, active, attention}; m.treeCountsValid = true }()
	add := func(n subagent.Node) {
		total++
		if isActiveSubagentState(n.State) {
			active++
		}
		if n.NeedsAttention {
			attention++
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
	return total, active, attention
}

func (m *model) treeSummary(width int) string {
	total, active, attention := m.treeCounts()
	if total == 0 || width < 1 {
		return ""
	}
	parts := []string{styles.TabPrimaryStyle.Render(fmt.Sprintf("%d subagents", total))}
	for i, value := range []int{active, attention} {
		counter := m.treeCounters[i]
		alpha := counter.alpha
		if !counter.running && value > 0 {
			alpha = 1
			counter.value = value
		}
		if alpha <= 0 || counter.value == 0 {
			continue
		}
		label := "active"
		if i == 1 {
			label = "attention"
		}
		parts = append(parts, styles.FadeLine(styles.MutedStyle.Render(fmt.Sprintf("%d %s", counter.value, label)), alpha))
	}
	glyph := "⌄"
	if m.treeCollapsed {
		glyph = "›"
	}
	if width == 1 {
		return styles.MutedStyle.Render(glyph)
	}
	text := ansi.Truncate(strings.Join(parts, " "), width-2, "…")
	return text + strings.Repeat(" ", max(1, width-ansi.StringWidth(text)-1)) + styles.MutedStyle.Render(glyph)
}

func (m *model) treeControlAt(x, y int) (treeControl, bool) {
	if m.placement != nil {
		return m.placementControlAt(x, y)
	}
	if m.layoutDirty {
		m.View()
	}
	if m.mode != ModeVertical || y < 0 || y >= m.viewportHeight() || x < m.layoutCfg.PaddingLeft {
		return treeControl{}, false
	}
	width := m.contentWidth(m.cachedNeedsScrollbar)
	x -= m.layoutCfg.PaddingLeft
	if x >= width {
		return treeControl{}, false
	}
	contentY := y + m.scrollview.ScrollOffset()
	row := contentY - m.treeSectionStart
	if contentY == m.todoSummaryLine && m.todoSummaryLine >= 0 {
		return treeControl{whole: true, todos: true}, true
	}
	if contentY == m.summaryLine && m.hasTreeContent() {
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

func (m *model) toggleTreeControl(control treeControl) tea.Cmd {
	if control.whole {
		if control.todos {
			m.todosCollapsed = !m.todosCollapsed
		} else {
			m.treeCollapsed = !m.treeCollapsed
		}
		m.invalidateCache()
		return m.ReconcileLayout()
	} else {
		if m.collapsedBranches == nil {
			m.collapsedBranches = make(map[subagent.NodeID]bool)
		}
		m.collapsedBranches[control.id] = !m.collapsedBranches[control.id]
	}

	m.invalidateCache()
	return m.ReconcileLayout()
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
	line := styles.MutedStyle.Render("  ") + m.hoverText(styles.AgentIdentityStyle(name, false).Render(ansi.Truncate(name, room, "…")), "agent:"+name)
	if reading != "" {
		line = padRight(line, width-lipgloss.Width(reading)) + contextGaugeStyle(m.agentContextGaugeLevel(name), styles.MutedStyle).Render(reading)
	}
	return ansi.Truncate(line, width, "")
}
