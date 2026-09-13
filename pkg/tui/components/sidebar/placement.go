package sidebar

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type placedRow struct {
	id, text                                         string
	y, fromY, targetY, alpha, fromAlpha, targetAlpha float64
	action                                           ClickResult
	payload                                          string
	contextWidth                                     int
	controls                                         []treeControl
	target                                           bool
	queueRemove                                      bool
}
type placementState struct {
	rows    []placedRow
	elapsed time.Duration
	running bool
	width   int
}

func (m *model) targetRows() []placedRow {
	width := m.contentWidth(false)
	lines := m.renderSections(width)
	m.cachedNeedsScrollbar = len(lines) > m.viewportHeight()
	if m.cachedNeedsScrollbar {
		width = m.contentWidth(true)
		lines = m.renderSections(width)
	}
	m.cachedLines = slices.Clone(lines)
	m.cachedWidth = m.contentWidth(false)
	m.layoutDirty = false
	rows := make([]placedRow, 0, len(lines))
	for y, text := range lines {
		row := placedRow{id: fmt.Sprintf("misc:%d", y), text: text, y: float64(y), targetY: float64(y), alpha: 1, targetAlpha: 1, target: true}
		switch {
		case y < m.titleLineCount():
			row.id = fmt.Sprintf("title:%d", y)
			row.action = ClickTitle
		case y == m.workingDirRow:
			row.id = "directory"
			row.action = ClickWorkingDir
		case y == m.workingDirRow+1 && m.gitBranchName != "":
			row.id = "branch"
		case y >= m.usageReadingLine && y < m.usageSectionEnd && m.usageReadingLine >= 0:
			row.id = fmt.Sprintf("usage:%d", y-m.usageReadingLine)
			row.action = ClickUsage
			if y == m.usageReadingLine {
				row.action = ClickUsageContext
				row.contextWidth = m.usageContextSegWidth
			}
		case y >= m.queueStart && y < m.queueEnd:
			i := y - m.queueStart
			meta := m.queueRows[i]
			row.id = meta.id
			if meta.turnID != "" {
				row.action = ClickQueuedMessage
				row.payload = meta.turnID
				row.queueRemove = meta.first
			}
		case y >= m.modelStart && y < m.modelEnd:
			row.id = fmt.Sprintf("model:%s:%d", m.activeAgentName(), y-m.modelStart)
			row.action = ClickModel
		case y == m.summaryLine && m.hasTreeContent():
			row.id = "tree-summary"
			row.controls = []treeControl{{x: width - 1, whole: true}}
		case y == m.parentLineZone:
			row.id = "parent:" + m.parentSessionID
			row.action = ClickSubagentParent
			row.payload = m.parentSessionID
		}
		if id, ok := m.subagentHoverZone[y]; ok {
			row.id = "node:" + string(id)
			row.action = ClickSubagent
			row.payload = string(id)
		}
		if name, ok := m.agentClickZones[y]; ok {
			row.id = "agent:" + name
			row.action = ClickAgent
			row.payload = name
		}
		if controls := m.treeControls[y-m.treeSectionStart]; len(controls) > 0 {
			row.controls = slices.Clone(controls)
		}
		if strings.TrimSpace(ansi.Strip(text)) == "" {
			row.id = "gap:" + row.id
			row.action = ClickNone
		}
		rows = append(rows, row)
	}
	for i := range rows {
		if strings.HasPrefix(rows[i].id, "gap:") {
			next := "end"
			for j := i + 1; j < len(rows); j++ {
				if !strings.HasPrefix(rows[j].id, "gap:") {
					next = rows[j].id
					break
				}
			}
			rows[i].id = "gap:" + next
		}
	}
	return rows
}

// ReconcileLayout runs at semantic update boundaries, never while painting.
func (m *model) ReconcileLayout() tea.Cmd {
	m.syncViewState()
	if m.mode != ModeVertical {
		return nil
	}
	if !m.reconcileDirty && !m.cacheDirty && m.placement != nil {
		return nil
	}
	counterCmd := tea.Batch(m.syncCounters(), m.syncBranchSpans())
	target := m.targetRows()
	m.reconcileDirty = false
	m.cacheDirty = false
	width := m.contentWidth(m.cachedNeedsScrollbar)
	if m.placement == nil || !m.presentationActive {
		m.placement = &placementState{rows: target, width: width}
		m.preparePlacementViewport()
		return counterCmd
	}
	current := m.placement
	expandingTree := !m.treeCollapsed
	if previousSummary, ok := findPlaced(current.rows, "tree-summary"); ok {
		expandingTree = expandingTree && strings.Contains(ansi.Strip(previousSummary.text), "›")
	} else {
		expandingTree = false
	}
	old := make(map[string]placedRow, len(current.rows))
	for _, row := range current.rows {
		old[row.id] = row
	}
	changed := false
	rows := make([]placedRow, 0, len(target)+len(old))
	for _, row := range target {
		if prior, ok := old[row.id]; ok {
			row.y, row.alpha = prior.y, prior.alpha
			changed = changed || prior.targetY != row.targetY || !prior.target
			delete(old, row.id)
		} else {
			row.alpha = 0
			if len(rows) > 0 {
				row.y = rows[len(rows)-1].y + 1
			}
			if expandingTree && (strings.HasPrefix(row.id, "node:") || strings.HasPrefix(row.id, "agent:") || strings.HasPrefix(row.id, "gap:node:") || strings.HasPrefix(row.id, "gap:agent:")) {
				survivors := make([]placedRow, 0, len(current.rows))
				for _, prior := range current.rows {
					if prior.target {
						survivors = append(survivors, prior)
					}
				}
				if len(survivors) > 0 {
					row.y = survivors[min(len(survivors)-1, max(0, int(row.targetY)))].y
				}
			}
			changed = true
		}
		row.fromY, row.fromAlpha = row.y, row.alpha
		rows = append(rows, row)
	}
	for _, prior := range current.rows {
		if _, ok := old[prior.id]; !ok {
			continue
		}
		if prior.target {
			changed = true
		}
		if prior.alpha <= 0 {
			continue
		}
		prior.fromY, prior.fromAlpha = prior.y, prior.alpha
		prior.target = false
		prior.targetAlpha = 0
		if len(target) > 0 {
			anchor := target[min(len(target)-1, max(0, int(prior.targetY)))]
			prior.targetY = anchor.targetY
		}
		prior.action = ClickNone
		prior.controls = nil
		rows = append(rows, prior)
	}
	if !changed {
		// Preserve elapsed interpolation while replacing only styled cell content.
		for i := range rows {
			if prior, ok := findPlaced(current.rows, rows[i].id); ok {
				rows[i].fromY = prior.fromY
				rows[i].fromAlpha = prior.fromAlpha
			}
		}
	} else {
		current.elapsed = 0
		current.running = true
		m.visualGeneration++
	}
	current.rows, current.width = rows, width
	m.preparePlacementViewport()
	if changed {
		return tea.Batch(counterCmd, m.presentationSub.Start())
	}
	return counterCmd
}

func findPlaced(rows []placedRow, id string) (placedRow, bool) {
	for _, row := range rows {
		if row.id == id {
			return row, true
		}
	}
	return placedRow{}, false
}

func (m *model) tickPlacement(tick animation.TickMsg) {
	before, after := tick.ElapsedBounds()
	delta := after - before
	changed := m.tickBranchSpans(delta)
	if changed {
		m.preparedTrees = nil
		m.cacheDirty = true
	}
	if m.placement != nil && m.placement.running {
		m.placement.elapsed += delta
		p := min(1, float64(m.placement.elapsed)/float64(animation.ShortDuration))
		eased := animation.EaseOutCubic(p)
		for i := range m.placement.rows {
			row := &m.placement.rows[i]
			row.y = row.fromY + (row.targetY-row.fromY)*eased
			row.alpha = row.fromAlpha + (row.targetAlpha-row.fromAlpha)*eased
		}
		if p >= 1 {
			m.placement.running = false
		}
		changed = true
	}
	for i := range m.treeCounters {
		counter := &m.treeCounters[i]
		if !counter.running {
			continue
		}
		counter.elapsed += delta
		p := min(1, float64(counter.elapsed)/float64(animation.ShortDuration))
		counter.alpha = counter.from + (counter.target-counter.from)*animation.EaseOutCubic(p)
		if p >= 1 {
			counter.running = false
		}
		changed = true
	}
	if (m.placement == nil || !m.placement.running) && !m.countersRunning() && !m.branchSpansRunning() {
		m.presentationSub.Stop()
	}
	if changed {
		m.preparePlacementViewport()
		m.visualGeneration++
		tick.MarkDirty()
	}
}

func (m *model) paintedRows() map[int]placedRow {
	rows := make(map[int]placedRow)
	if m.placement == nil {
		return rows
	}
	// Exiting rows paint first; target order wins collisions deterministically.
	for _, target := range []bool{false, true} {
		for _, row := range m.placement.rows {
			if row.target != target || row.alpha <= 0 {
				continue
			}
			rows[int(math.Round(row.y))] = row
		}
	}
	// Persistent navigation cannot be covered while emerging children still round
	// to the collapse anchor; paint and hit ownership use this same winner map.
	for _, row := range m.placement.rows {
		if row.target && row.alpha > 0 && (row.id == "tree-summary" || strings.HasPrefix(row.id, "parent:")) {
			rows[int(math.Round(row.y))] = row
		}
	}
	return rows
}

func (m *model) placementView() string {
	height := m.viewportHeight()
	painted := m.paintedRows()
	fresh := map[string]string{}
	if styles.ThemeGeneration() != m.themeGeneration {
		copyModel := *m
		copyModel.themeGeneration = styles.ThemeGeneration()
		copyModel.sectionCache = nil
		copyModel.preparedTrees = nil
		copyModel.placement = nil
		todos := *m.todoComp
		todos.InvalidateCache()
		copyModel.todoComp = &todos
		copyModel.renderedActiveAgent = copyModel.activeAgentName()
		for _, row := range copyModel.targetRows() {
			fresh[row.id] = row.text
		}
	}
	total := m.placementExtent()
	offset := m.scrollview.ScrollOffset()
	width := m.contentWidth(m.cachedNeedsScrollbar)
	lines := make([]string, height)
	for i := range height {
		row, ok := painted[offset+i]
		text := ""
		if ok {
			text = row.text
			if updated, found := fresh[row.id]; found {
				text = updated
			}
			if row.id == "tree-summary" {
				text = m.hoverText(m.treeSummary(width), "tree-summary")
			}
			text = styles.FadeLine(text, row.alpha)
		}
		lines[i] = padRight(ansi.Truncate(text, width, ""), width)
	}
	view := ""
	if height > 0 {
		viewport := scrollview.New(scrollview.WithGapWidth(m.layoutCfg.ScrollbarGap), scrollview.WithKeyMap(nil))
		viewport.SetSize(m.width-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, height)
		viewport.SetContent(make([]string, total), total)
		viewport.SetScrollOffset(offset)
		view = viewport.ViewWithPaddedLines(lines)
	}
	if m.footerHeight() > 0 {
		if view != "" {
			view += "\n"
		}
		view += m.footerView(m.contentWidth(false))
	}
	return view
}

func (m *model) placementExtent() int {
	total := 0
	if m.placement == nil {
		return total
	}
	for _, row := range m.placement.rows {
		if row.alpha > 0 || row.target {
			total = max(total, int(math.Ceil(max(row.y, row.targetY)))+1)
		}
	}
	return total
}

func (m *model) preparePlacementViewport() {
	total := m.placementExtent()
	m.scrollview.SetSize(m.width-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, m.viewportHeight())
	m.scrollview.SetContent(make([]string, total), total)
}

func (m *model) placementRowAt(x, y int) (placedRow, bool) {
	if y < 0 || y >= m.viewportHeight() || x < m.layoutCfg.PaddingLeft || x >= m.layoutCfg.PaddingLeft+m.contentWidth(m.cachedNeedsScrollbar) {
		return placedRow{}, false
	}
	row, ok := m.paintedRows()[y+m.scrollview.ScrollOffset()]
	return row, ok && row.target && row.alpha > 0
}

func (m *model) placementClick(x, y int) (ClickResult, string) {
	if m.branchCapture.id != "" && m.branchCapture.x == x && m.branchCapture.y == y {
		return ClickNone, ""
	}
	row, ok := m.placementRowAt(x, y)
	if !ok {
		return ClickNone, ""
	}
	col := x - m.layoutCfg.PaddingLeft
	if row.action == ClickWorkingDir && col == m.contentWidth(m.cachedNeedsScrollbar)-1 {
		return ClickNone, ""
	}
	if row.action == ClickWorkingDir && m.directoryIconHit(col, m.contentWidth(m.cachedNeedsScrollbar)) {
		return ClickOpenWorkingDir, m.WorkingDirectory()
	}
	if row.queueRemove && col == m.contentWidth(m.cachedNeedsScrollbar)-1 {
		return ClickRemoveQueuedMessage, row.payload
	}
	for _, control := range row.controls {
		if control.whole || col == control.x {
			return ClickNone, ""
		}
	}
	if row.action == ClickUsageContext && col >= row.contextWidth {
		return ClickUsage, ""
	}
	if row.action == ClickTitle {
		if y+m.scrollview.ScrollOffset() == 0 && m.sessionHasContent && col <= starClickWidth {
			return ClickStar, ""
		}
		if !m.titleGenerated || m.editingTitle {
			return ClickNone, ""
		}
	}
	return row.action, row.payload
}

func (m *model) placementControlAt(x, y int) (treeControl, bool) {
	if control, ok := m.capturedBranchAt(x, y); ok {
		return control, true
	}
	row, ok := m.placementRowAt(x, y)
	if !ok {
		return treeControl{}, false
	}
	for _, control := range row.controls {
		if control.whole || x-m.layoutCfg.PaddingLeft == control.x {
			return control, true
		}
	}
	return treeControl{}, false
}
