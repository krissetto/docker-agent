package sidebar

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
)

func (m *model) hasTreeContent() bool {
	return len(m.subagentNodes) > 0 || len(m.participantNames()) > 0
}

func (m *model) updateRegionHover(x, y int) tea.Cmd {
	if m.placement != nil && m.mode == ModeVertical {
		row, ok := m.placementRowAt(x, y)
		if ok && row.action == ClickSubagent {
			m.hoveredSubagent = subagent.NodeID(row.payload)
		}
	}
	region, payload := m.HandleClickType(x, y)
	key := ""
	if row, ok := m.placementRowAt(x, y); ok && row.action == ClickWorkingDir {
		key = "directory"
	}
	if control, ok := m.treeControlAt(x, y); ok {
		key = "tree-summary"
		if control.todos {
			key = "todo-summary"
		} else if !control.whole {
			key = "node:" + string(control.id)
		}
	}
	switch region {
	case ClickWorkingDir:
		key = "directory"
	case ClickOpenWorkingDir:
		key = "directory-open"
	case ClickUsageContext:
		key = "context"
	case ClickUsage:
		key = "cost"
	case ClickModel:
		key = "model"
	case ClickThinkingLevel:
		key = "thinking-level"
	case ClickQueuedMessage:
		key = "queue:" + payload
	case ClickRemoveQueuedMessage:
		key = "queue-remove:" + payload
	case ClickAgent:
		key = "agent:" + payload
		if m.delegationRoot != nil && payload == m.delegationRoot.Agent {
			key = "node:" + string(m.delegationRoot.ID)
		}
	case ClickSubagent:
		key = "node:" + payload
	case ClickSubagentParent:
		key = "parent:" + payload
	default:
		region = ClickNone
	}
	m.hoveredRegion = region
	return m.setHoverTarget(key)
}

func (m *model) decoratePane(lines []string, width int) {
	if m.workingDirRow >= 0 && m.workingDirRow < len(lines) {
		lines[m.workingDirRow] = m.directoryRow(width)
	}
	for row := m.modelStart; row < m.modelEnd; row++ {
		lines[row] = m.modelRowView(width, row-m.modelStart)
	}
	if m.usageReadingLine >= 0 && m.usageReadingLine < len(lines) {
		row := m.usageReadingLine
		cut := min(m.usageContextSegWidth, width)
		left, right := ansi.Cut(lines[row], 0, cut), ansi.Cut(lines[row], cut, width)
		lines[row] = m.hoverText(left, "context") + m.hoverText(right, "cost")
		for row++; row < m.usageSectionEnd; row++ {
			lines[row] = m.hoverText(lines[row], "cost")
		}
	}
}

// Insert at most one row between semantic blocks, only when the viewport has room.
func (m *model) addBreathingRows(lines []string, boundaries []int) []string {
	offsets := make(map[int]int, len(boundaries))
	for _, row := range boundaries {
		offsets[row] = 1
	}
	shifted := make(map[int]int, len(lines)+1)
	result := make([]string, 0, len(lines)+len(boundaries))
	for i, line := range lines {
		if offsets[i] != 0 {
			result = append(result, "")
		}
		shifted[i] = len(result)
		result = append(result, line)
	}
	shifted[len(lines)] = len(result)
	shiftEnd := func(end int) int {
		if end <= 0 {
			return end
		}
		return shifted[end-1] + 1
	}
	m.usageSectionEnd = shiftEnd(m.usageSectionEnd)
	if m.usageReadingLine >= 0 {
		m.usageReadingLine = shifted[m.usageReadingLine]
	}
	m.queueEnd = shiftEnd(m.queueEnd)
	m.queueStart = shifted[m.queueStart]
	if m.agentIdentityRow >= 0 {
		m.agentIdentityRow = shifted[m.agentIdentityRow]
	}
	m.modelEnd = shiftEnd(m.modelEnd)
	m.modelStart = shifted[m.modelStart]
	if m.todoSummaryLine >= 0 {
		m.todoSummaryLine = shifted[m.todoSummaryLine]
		m.todoEnd = shiftEnd(m.todoEnd)
	}
	m.treeSectionStart = shifted[m.treeSectionStart]
	m.summaryLine = shifted[m.summaryLine]
	if m.workingDirRow >= 0 {
		m.workingDirRow = shifted[m.workingDirRow]
	}
	agents := make(map[int]string, len(m.agentClickZones))
	for row, name := range m.agentClickZones {
		agents[shifted[row]] = name
	}
	m.agentClickZones = agents
	nodes := make(map[int]subagent.NodeID, len(m.subagentHoverZone))
	for row, id := range m.subagentHoverZone {
		nodes[shifted[row]] = id
	}
	m.subagentHoverZone = nodes
	if m.parentLineZone >= 0 {
		m.parentLineZone = shifted[m.parentLineZone]
	}
	return result
}
