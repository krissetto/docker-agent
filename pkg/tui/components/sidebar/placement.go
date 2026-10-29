package sidebar

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/review"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type placedRow struct {
	identity                                         identityColumns
	id, text                                         string
	y, fromY, targetY, alpha, fromAlpha, targetAlpha float64
	action                                           ClickResult
	payload                                          string
	contextWidth                                     int
	controls                                         []treeControl
	target                                           bool
	queueControls                                    bool
	todoControls                                     bool
}
type paddedPlacementRow struct {
	text, padded string
	alpha        float64
	width        int
}

type placementState struct {
	rows               []placedRow
	elapsed            time.Duration
	running            bool
	width              int
	painted            map[int]placedRow
	paintedCount       int
	paintedOffset      int
	extent             int
	targetByID         map[string]int
	dynamicRows        []int
	dynamicNonTreeRows []int
	treeNodeRows       map[subagent.NodeID]int
	activeTreeRows     map[subagent.NodeID]bool
	visibleTreeRows    []subagent.NodeID
	visibleTreeSet     map[subagent.NodeID]bool
	visibleTreeOffset  int
	runningSpanIDs     []subagent.NodeID
	runningSpanSet     map[subagent.NodeID]bool
	spanIndexValid     bool
	treeAnimationDirty bool
	hoveredBranchID    subagent.NodeID
	hoveredTreeID      subagent.NodeID
	pendingTreeRows    map[subagent.NodeID]struct{}
	animationTreeRows  map[subagent.NodeID]struct{}
	treeRowRefreshes   uint64
	crossSession       bool
	padded             []paddedPlacementRow
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
				row.queueControls = meta.first
			}
		case y == m.agentIdentityRow:
			row.id = "active-agent"
		case y >= m.modelStart && y < m.modelEnd:
			row.id = fmt.Sprintf("model:%s:%d", m.activeAgentName(), y-m.modelStart)
			row.action = ClickModel
		case y == m.todoSummaryLine && m.todoSummaryLine >= 0:
			row.id = "todo-summary"
			row.controls = []treeControl{{x: width - 1, whole: true, todos: true}}
		case y > m.todoSummaryLine && y < m.todoEnd && m.todoSummaryLine >= 0:
			line := y - m.todoSummaryLine - 2
			row.id = fmt.Sprintf("todo:%d", line)
			if item, ok := m.todoComp.TodoAtLine(line); ok {
				row.id = fmt.Sprintf("todo:%s:%d", item.ID, m.todoComp.OffsetAtLine(line))
				row.payload = item.ID
				row.todoControls = m.todoComp.ControlsAtLine(line)
			}
		case y == m.summaryLine && m.hasTreeContent():
			row.id = "tree-summary"
			row.controls = []treeControl{{x: width - 1, whole: true}}
		case m.backgroundStart >= 0 && y == m.backgroundHeader:
			row.id = "background:header"
		case m.backgroundMoreLine >= 0 && y == m.backgroundMoreLine:
			row.id = "background:more"
		case m.backgroundStart >= 0 && y > m.backgroundHeader && y < m.backgroundEnd:
			index := y - m.backgroundStart - 1
			if index >= 0 && index < len(m.backgroundRowIDs) {
				row.id = "background:process:" + m.backgroundRowIDs[index]
				row.payload = m.backgroundRowTools[index]
				if row.payload != "" {
					row.action = ClickBackgroundProcess
				}
			}
		case m.gitStart >= 0 && y >= m.gitStart && y < m.gitEnd:
			line := y - m.gitStart
			row.id = m.git.rowID(line)
			if m.git.interactive(line) {
				row.action = ClickGit
				row.payload = strconv.Itoa(line)
			}
		case y == m.parentLineZone:
			row.id = "parent:" + m.parentSessionID
			row.action = ClickSubagentParent
			row.payload = m.parentSessionID
		}
		if id, ok := m.subagentHoverZone[y]; ok {
			row.id = "node:" + string(id)
			row.action = ClickSubagent
			row.payload = string(id)
			row.identity = m.preparedTrees[width].rows[y-m.treeSectionStart].identity
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
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.HasPrefix(rows[i].id, "gap:") {
			next := "end"
			if i+1 < len(rows) {
				next = rows[i+1].id
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
		return m.syncBackgroundAnimation()
	}
	if m.placement == nil && !m.reconcileDirty && !m.layoutDirty && m.backgroundOnlyDirty {
		m.refreshBackgroundMotion()
		m.backgroundOnlyDirty = false
		m.cacheDirty = false
		return nil
	}
	if !m.reconcileDirty && !m.layoutDirty && m.backgroundOnlyDirty {
		m.refreshBackgroundMotion()
		m.backgroundOnlyDirty = false
		m.cacheDirty = false
		return nil
	}
	if !m.reconcileDirty && !m.cacheDirty && m.placement != nil {
		return nil
	}
	if !m.reconcileDirty && !m.layoutDirty && m.placement != nil {
		switch {
		case m.hoverOnlyDirty && m.backgroundOnlyHover():
			m.refreshBackgroundMotion()
		case m.hoverOnlyDirty:
			m.refreshPlacementHover()
			m.placement.treeAnimationDirty = false
		case m.placement.treeAnimationDirty || m.treeAnimationOnly():
			m.refreshPlacementTreeAnimation()
			m.placement.treeAnimationDirty = false
		default:
			m.refreshPlacementPresentation()
		}
		m.cacheDirty = false
		return nil
	}
	counterCmd := tea.Batch(m.syncCounters(), m.syncBranchSpans())
	target := m.targetRows()
	m.reconcileDirty = false
	m.cacheDirty = false
	width := m.contentWidth(m.cachedNeedsScrollbar)
	if m.placement == nil || !m.presentationActive {
		m.placement = &placementState{rows: target, width: width, targetByID: targetPlacementIndexes(target)}
		m.placement.extent = placementRowsExtent(target)
		m.indexPlacementRows()
		m.preparePlacementViewport()
		return tea.Batch(counterCmd, m.syncBackgroundAnimation())
	}
	if m.placement.crossSession {
		m.preserveChangedRows(target)
	}
	m.placement.crossSession = false
	current := m.placement
	expanding := func(summary string, collapsed bool) bool {
		previous, ok := findPlaced(current.rows, summary)
		return !collapsed && ok && strings.Contains(ansi.Strip(previous.text), "›")
	}
	expandingTree := expanding("tree-summary", m.treeCollapsed)
	expandingTodos := expanding("todo-summary", m.todosCollapsed)
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
			id := strings.TrimPrefix(row.id, "gap:")
			if (expandingTree && (strings.HasPrefix(id, "node:") || strings.HasPrefix(id, "agent:"))) ||
				(expandingTodos && strings.HasPrefix(id, "todo:")) {
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
		previous := make(map[string]placedRow, len(current.rows))
		for _, row := range current.rows {
			previous[row.id] = row
		}
		for i := range rows {
			if prior, ok := previous[rows[i].id]; ok {
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
	current.targetByID = targetPlacementIndexes(rows)
	current.extent = placementRowsExtent(rows)
	m.indexPlacementRows()
	m.preparePlacementViewport()
	if changed {
		return tea.Batch(counterCmd, m.presentationSub.Start())
	}
	return tea.Batch(counterCmd, m.syncBackgroundAnimation())
}

func (m *model) indexPlacementRows() {
	if m.placement == nil {
		return
	}
	p := m.placement
	p.dynamicRows = p.dynamicRows[:0]
	p.dynamicNonTreeRows = p.dynamicNonTreeRows[:0]
	p.treeNodeRows = make(map[subagent.NodeID]int)
	p.activeTreeRows = make(map[subagent.NodeID]bool)
	prepared := m.preparedTrees[p.width]
	for i, row := range p.rows {
		if !row.target || row.action == ClickGit || strings.HasPrefix(row.id, "git:") {
			continue
		}
		p.dynamicRows = append(p.dynamicRows, i)
		if row.action == ClickSubagent {
			id := subagent.NodeID(row.payload)
			p.treeNodeRows[id] = i
			if line, ok := prepared.rowByID[id]; ok {
				if item, exists := prepared.rows[line]; exists && (isActiveSubagentState(item.node.State) || hasSubagentBackgroundActivity(item.node)) {
					p.activeTreeRows[id] = true
				}
			}
			continue
		}
		p.dynamicNonTreeRows = append(p.dynamicNonTreeRows, i)
	}
	p.runningSpanIDs, p.runningSpanSet = runningBranchSpanIndex(m.branchSpans)
	p.spanIndexValid = true
	p.hoveredBranchID = m.branchHoverNodeID()
	p.hoveredTreeID = m.hoveredSubagent
	if p.pendingTreeRows == nil {
		p.pendingTreeRows = make(map[subagent.NodeID]struct{})
	}
	if p.animationTreeRows == nil {
		p.animationTreeRows = make(map[subagent.NodeID]struct{})
	}
	if p.visibleTreeSet == nil {
		p.visibleTreeSet = make(map[subagent.NodeID]bool)
	}
	p.visibleTreeOffset = -1
}

func refreshablePlacementIndexes(rows []placedRow) []int {
	indexes := make([]int, 0, len(rows))
	for i, row := range rows {
		if row.target && row.action != ClickGit && !strings.HasPrefix(row.id, "git:") {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func targetPlacementIndexes(rows []placedRow) map[string]int {
	indexes := make(map[string]int, len(rows))
	for i, row := range rows {
		if row.target {
			indexes[row.id] = i
		}
	}
	return indexes
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
		if m.treeAnimationOnly() {
			// Tree-row animation owns a narrow cache refresh. Without marking it
			// dirty here, a tick with no spinner-frame change never reaches the
			// presentation reconciler and leaves the last branch-span frame painted.
			if !m.cacheDirty {
				m.cacheDirty = true
			}
			m.placement.treeAnimationDirty = true
		} else {
			m.cacheDirty = true
		}
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
			m.placement.rows = slices.DeleteFunc(m.placement.rows, func(row placedRow) bool { return !row.target })
			m.placement.targetByID = targetPlacementIndexes(m.placement.rows)
			m.indexPlacementRows()
			m.placement.extent = placementRowsExtent(m.placement.rows)
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
	if m.placement == nil {
		return nil
	}
	offset := m.scrollview.ScrollOffset()
	if m.placement.painted != nil && m.placement.paintedCount == len(m.placement.rows) && m.placement.paintedOffset == offset {
		return m.placement.painted
	}
	rows := make(map[int]placedRow, m.viewportHeight()+2)
	add := func(row placedRow) {
		if row.alpha > 0 {
			rows[int(math.Round(row.y))] = row
		}
	}
	if !m.placement.running {
		// Settled target rows are already ordered by targetY. Probe only the
		// visible range so pointer hit-testing and painting stay independent of
		// total tree/file count. During structural transitions use the full
		// collision pass below because entering/exiting rows can cross anchors.
		startY, endY := float64(max(0, offset-1)), float64(offset+m.viewportHeight()+1)
		start := sort.Search(len(m.placement.rows), func(i int) bool { return m.placement.rows[i].targetY >= startY })
		for i := start; i < len(m.placement.rows); i++ {
			row := m.placement.rows[i]
			if row.targetY > endY {
				break
			}
			if row.target {
				add(row)
			}
		}
	} else {
		// Exiting rows paint first; target order wins collisions deterministically.
		for _, target := range []bool{false, true} {
			for _, row := range m.placement.rows {
				if row.target != target || row.alpha <= 0 {
					continue
				}
				add(row)
			}
		}
		// Persistent navigation cannot be covered while emerging children still round
		// to the collapse anchor; paint and hit ownership use this same winner map.
		for _, row := range m.placement.rows {
			if row.target && row.alpha > 0 && (row.id == "tree-summary" || row.id == "todo-summary" || strings.HasPrefix(row.id, "parent:")) {
				rows[int(math.Round(row.y))] = row
			}
		}
	}
	m.placement.painted = rows
	m.placement.paintedCount = len(m.placement.rows)
	m.placement.paintedOffset = offset
	return rows
}

func (m *model) placementView() string {
	height := m.viewportHeight()
	m.refreshVisibleTreeRows()
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
	if len(m.placement.padded) != height {
		m.placement.padded = make([]paddedPlacementRow, height)
	}
	for i := range height {
		row, ok := painted[offset+i]
		text := ""
		if ok {
			text = row.text
			if updated, found := fresh[row.id]; found {
				row.text = updated
			}
			text = m.placementText(row, width)
		}
		cached := &m.placement.padded[i]
		if cached.width != width || cached.text != text || cached.alpha != row.alpha {
			*cached = paddedPlacementRow{
				text: text, alpha: row.alpha, width: width,
				padded: padRight(ansi.Truncate(styles.FadeLine(text, row.alpha), width, ""), width),
			}
		}
		lines[i] = cached.padded
	}
	view := ""
	if height > 0 {
		// Paint the same scrollbar that owns pointer capture, including its active
		// thumb color on press/release without an offset change.
		viewport := m.scrollview
		viewport.SetSize(m.width-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, height)
		viewport.SetContent(nil, total)
		viewport.SetScrollOffset(offset)
		view = viewport.ViewWithPaddedLines(lines)
		if view == "" {
			view = strings.Repeat("\n", height-1)
		}
	}
	if m.footerHeight() > 0 {
		if height > 0 {
			view += "\n"
		}
		view += strings.Repeat("\n", m.footerGapHeight()) + m.footerView(m.contentWidth(false))
	}
	return view
}

func (m *model) placementExtent() int {
	if m.placement == nil {
		return 0
	}
	return m.placement.extent
}

func placementRowsExtent(rows []placedRow) int {
	total := 0
	for _, row := range rows {
		if row.alpha > 0 || row.target {
			total = max(total, int(math.Ceil(max(row.y, row.targetY)))+1)
		}
	}
	return total
}

func (m *model) preparePlacementViewport() {
	if m.placement != nil {
		m.placement.painted = nil
	}
	total := m.placementExtent()
	m.scrollview.SetSize(m.width-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, m.viewportHeight())
	m.scrollview.SetContent(nil, total)
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
	if row.action == ClickModel {
		return m.modelClick(col, modelPlacementRow(row.id), m.contentWidth(m.cachedNeedsScrollbar)), ""
	}
	if row.action == ClickGit && row.id == "git:summary" {
		mode, ok := m.git.modeAtX(col)
		if ok {
			if mode == review.ModeFiles {
				return ClickGit, "mode:browse"
			}
			return ClickGit, "mode:changes"
		}
		return ClickGit, "0"
	}
	if row.action == ClickQueuedMessage {
		indent, actions := rowActions(m.contentWidth(m.cachedNeedsScrollbar), false)
		switch actions.PartAt(col-indent, row.queueControls) {
		case "edit":
			return ClickEditQueuedMessage, row.payload
		case "remove":
			return ClickRemoveQueuedMessage, row.payload
		}
	}
	for _, control := range row.controls {
		if control.whole || (col == control.x && row.chevronAt(col)) {
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
		if control.whole || (x-m.layoutCfg.PaddingLeft == control.x && row.chevronAt(control.x)) {
			return control, true
		}
	}
	return treeControl{}, false
}

func (r placedRow) chevronAt(x int) bool {
	glyph := ansi.Strip(ansi.Cut(r.text, x, x+1))
	return glyph == "⌄" || glyph == "›"
}

func modelPlacementRow(id string) int {
	if strings.HasSuffix(id, ":1") {
		return 1
	}
	return 0
}

// Presentation updates retain row identities, layout, prepared topology and counters.
func (m *model) refreshPlacementPresentation() {
	width := m.placement.width
	lines := m.renderSections(width)
	m.cachedLines = lines
	for _, i := range m.placement.dynamicRows {
		row := &m.placement.rows[i]
		y := int(row.targetY)
		if row.target && y >= 0 && y < len(lines) {
			row.text = lines[y]
			if strings.HasPrefix(row.id, "node:") {
				row.controls = slices.Clone(m.treeControls[y-m.treeSectionStart])
				row.identity = m.preparedTrees[width].rows[y-m.treeSectionStart].identity
			}
		}
	}
	m.placement.painted = nil
	m.refreshGitSummaryFrame()
}

func (m *model) refreshGitHoverRows(width int) {
	if m.placement == nil || m.placement.targetByID == nil || m.gitStart < 0 {
		return
	}
	seen := make(map[int]bool, len(m.gitHoverLines))
	for _, line := range m.gitHoverLines {
		if seen[line] {
			continue
		}
		seen[line] = true
		id := m.git.rowID(line)
		index, ok := m.placement.targetByID[id]
		if !ok {
			continue
		}
		if line < 0 || line >= len(m.git.displayRows)+1 {
			continue
		}
		row := &m.placement.rows[index]
		row.text = m.git.line(width, line, m.hoverValues)
		y := int(row.targetY)
		if y >= 0 && y < len(m.cachedLines) {
			m.cachedLines[y] = row.text
		}
	}
}

// refreshTreeRows patches selected tree rows in the prepared section, placed rows,
// hit metadata, and cached full-height line buffer as one visual update.
func (m *model) refreshTreeRows(ids map[subagent.NodeID]struct{}) {
	p := m.placement
	if p == nil || len(ids) == 0 {
		return
	}
	width := p.width
	prepared, ok := m.preparedTrees[width]
	if !ok {
		return
	}
	changed := false
	for id := range ids {
		placed, found := p.treeNodeRows[id]
		line, hasLine := prepared.rowByID[id]
		if !found || !hasLine || placed < 0 || placed >= len(p.rows) {
			continue
		}
		item, found := prepared.rows[line]
		if !found {
			continue
		}
		p.treeRowRefreshes++
		// Candidate membership already proves this row's presentation may have
		// changed. Force row styling to regenerate from the current channels
		// rather than relying on the broad-channel equality snapshot here.
		item.text = ""
		// Make the style caches match their authoritative channel values before
		// recomputing the composite text (the ID span is painted neutral).
		item.hover = m.hoverValues["node:"+string(id)]
		item.nameHover = m.hoverValues["node-name:"+string(id)]
		item.span = m.branchSpans[id]
		item.hovered = m.hoveredSubagent == id
		item = m.refreshPreparedTreeRow(item, line, width)
		prepared.rows[line] = item
		row := &p.rows[placed]
		row.text, row.controls, row.identity = item.text, item.controls, item.identity
		m.treeControls[line] = slices.Clone(item.controls)
		changed = true
		y := int(row.targetY)
		if y >= 0 && y < len(m.cachedLines) {
			m.cachedLines[y] = item.text
		}
	}
	if changed {
		p.painted = nil
	}
}

func (m *model) refreshTreeSummary() {
	p := m.placement
	if p == nil {
		return
	}
	index, ok := p.targetByID["tree-summary"]
	if !ok || index < 0 || index >= len(p.rows) {
		return
	}
	row := &p.rows[index]
	line := int(row.targetY)
	text := m.hoverText(m.treeSummary(p.width), "tree-summary")
	row.text = text
	if line >= 0 && line < len(m.cachedLines) {
		m.cachedLines[line] = text
	}
}

func (m *model) preparedTreeBackgroundRunning(id subagent.NodeID, width int) bool {
	prepared, ok := m.preparedTrees[width]
	if !ok {
		return false
	}
	line, ok := prepared.rowByID[id]
	if !ok {
		return false
	}
	item, ok := prepared.rows[line]
	return ok && hasSubagentBackgroundActivity(item.node)
}

func (m *model) refreshTreeAnimationRows() {
	p := m.placement
	if p == nil {
		return
	}
	ids := p.animationTreeRows
	if ids == nil {
		ids = make(map[subagent.NodeID]struct{})
		p.animationTreeRows = ids
	}
	clear(ids)
	for _, id := range p.visibleTreeRows {
		if p.activeTreeRows[id] || p.runningSpanSet[id] || m.preparedTreeBackgroundRunning(id, p.width) {
			ids[id] = struct{}{}
		}
	}
	for _, id := range p.runningSpanIDs {
		if p.visibleTreeSet[id] {
			ids[id] = struct{}{}
		}
	}
	for id := range p.pendingTreeRows {
		if p.activeTreeRows[id] || p.runningSpanSet[id] || m.preparedTreeBackgroundRunning(id, p.width) || m.hoveredSubagent == id || m.hoverValues["node:"+string(id)].value != 0 || m.hoverValues["node-name:"+string(id)].value != 0 {
			ids[id] = struct{}{}
		}
	}
	if m.workingAgent != "" && m.delegationRoot != nil && m.delegationRoot.Agent == m.workingAgent && p.visibleTreeSet[m.delegationRoot.ID] {
		ids[m.delegationRoot.ID] = struct{}{}
	}
	m.refreshTreeRows(ids)
	p.animationTreeRows = ids
	m.refreshTreeSummary()
}

func (m *model) refreshVisibleTreeRows() {
	p := m.placement
	if p == nil || p.visibleTreeOffset == m.scrollview.ScrollOffset() {
		return
	}
	offset := m.scrollview.ScrollOffset()
	startY := float64(offset)
	endY := float64(offset + m.viewportHeight() - 1)
	start := sort.Search(len(p.rows), func(i int) bool { return p.rows[i].targetY >= startY })
	visible := p.visibleTreeRows[:0]
	ids := p.animationTreeRows
	if ids == nil {
		ids = make(map[subagent.NodeID]struct{})
		p.animationTreeRows = ids
	}
	clear(ids)
	for i := start; i < len(p.rows); i++ {
		row := p.rows[i]
		if row.targetY > endY {
			break
		}
		if row.target && row.action == ClickSubagent {
			id := subagent.NodeID(row.payload)
			visible = append(visible, id)
			if m.preparedTreeBackgroundRunning(id, p.width) {
				ids[id] = struct{}{}
			}
		}
	}
	p.visibleTreeRows = visible
	clear(p.visibleTreeSet)
	for _, id := range visible {
		p.visibleTreeSet[id] = true
		ids[id] = struct{}{}
	}
	p.visibleTreeOffset = offset
	m.refreshTreeRows(ids)
	m.refreshTreeSummary()
}

func (m *model) refreshPlacementTreeAnimation() {
	if !m.treeAnimationOnly() {
		m.refreshPlacementPresentation()
		return
	}
	m.refreshTreeAnimationRows()
	m.refreshGitSummaryFrame()
	m.sectionRenders++
}

func (m *model) refreshGitSummaryFrame() {
	p := m.placement
	if p == nil || (!m.git.data.Loading && len(m.git.data.Scanning) == 0) {
		return
	}
	index, ok := p.targetByID["git:summary"]
	if !ok || index < 0 || index >= len(p.rows) {
		return
	}
	row := &p.rows[index]
	line := int(row.targetY)
	row.text = m.git.summaryFrame(p.width, m.spinner.RawFrame(), m.hoverValues)
	if line >= 0 && line < len(m.cachedLines) {
		m.cachedLines[line] = row.text
	}
	p.painted = nil
}

// treeAnimationOnly admits only the row spinner channel. Every other sidebar
// spinner/transfer source can affect additional sections and keeps the full path.
func (m *model) treeAnimationOnly() bool {
	rootSpinnerRow := false
	if m.placement != nil && m.delegationRoot != nil && m.workingAgent != "" && m.delegationRoot.Agent == m.workingAgent {
		rootSpinnerRow = m.placement.activeTreeRows[m.delegationRoot.ID]
	}
	mainSpinnerOnlyTreeRow := m.workingAgent != "" && !m.toolsLoading && !m.titleRegenerating && !m.compacting && rootSpinnerRow
	gitSpinnerOnly := m.git.data.Loading || len(m.git.data.Scanning) > 0
	mainSpinnerOnlyTreeRow = mainSpinnerOnlyTreeRow || m.workingAgent == ""
	return m.placement != nil && (m.subagentSpinnerOn || m.countersRunning() || m.branchSpansRunning() || mainSpinnerOnlyTreeRow || gitSpinnerOnly) &&
		(m.workingAgent == "" || mainSpinnerOnlyTreeRow) && !m.toolsLoading && !m.titleRegenerating && !m.compacting &&
		len(m.ragIndexing) == 0 && !m.transferAnimation.IsActive()
}

func (m *model) refreshPlacementHover() {
	p := m.placement
	if p == nil {
		return
	}
	width := p.width
	if len(m.gitHoverLines) > 0 {
		m.refreshGitHoverRows(width)
		m.gitHoverLines = nil
	}
	prepared, ok := m.preparedTrees[width]
	if !ok {
		m.refreshPlacementPresentation()
		return
	}
	ids := p.pendingTreeRows
	if ids == nil {
		ids = make(map[subagent.NodeID]struct{})
	}
	add := func(id subagent.NodeID) {
		if id != "" {
			if _, exists := p.treeNodeRows[id]; exists {
				ids[id] = struct{}{}
			}
		}
	}
	add(p.hoveredTreeID)
	add(m.hoveredSubagent)
	add(p.hoveredBranchID)
	add(m.branchHoverNodeID())
	for _, id := range p.runningSpanIDs {
		if p.visibleTreeSet[id] {
			add(id)
		}
	}
	for key := range m.hoverValues {
		if strings.HasPrefix(key, "node:") {
			add(subagent.NodeID(strings.TrimPrefix(key, "node:")))
		} else if strings.HasPrefix(key, "node-name:") {
			add(subagent.NodeID(strings.TrimPrefix(key, "node-name:")))
		}
	}
	// Non-tree hover targets remain on their existing small dynamic-row path.
	for _, i := range p.dynamicNonTreeRows {
		row := &p.rows[i]
		if !row.target {
			continue
		}
		y := int(row.targetY)
		switch {
		case row.action == ClickWorkingDir:
			row.text = m.directoryRow(width)
		case row.action == ClickModel:
			// Model rows share the footer's hover path so pointer entry/exit repaints them.
			row.text = m.modelRowView(width, y-m.modelStart)
		case row.id == "todo-summary":
			row.text = m.todoSummary(width)
		case row.action == ClickSubagentParent:
			row.text = ansi.Truncate(m.parentLine(), width, "…")
		case row.action == ClickAgent:
			indent := min(2, max(0, width-1))
			row.text = strings.Repeat(" ", indent) + m.participantLine(row.payload, max(1, width-indent))
		case y >= m.usageReadingLine && y < m.usageSectionEnd && m.usageReadingLine >= 0:
			usage := strings.Split(m.sectionCache["usage"], "\n")
			index := y - m.usageReadingLine
			if index < len(usage) {
				row.text = m.hoverText(usage[index], "cost")
				if index == 0 {
					cut := min(m.usageContextSegWidth, width)
					row.text = m.hoverText(ansi.Cut(usage[0], 0, cut), "context") + m.hoverText(ansi.Cut(usage[0], cut, width), "cost")
				}
			}
		}
		if y >= 0 && y < len(m.cachedLines) {
			m.cachedLines[y] = row.text
		}
	}
	m.refreshTreeRows(ids)
	m.refreshTreeSummary()
	// Keep rows until the final zero frame has been copied into the cache.
	for id := range ids {
		line, exists := prepared.rowByID[id]
		if !exists {
			delete(ids, id)
			continue
		}
		_, itemExists := prepared.rows[line]
		if !itemExists {
			delete(ids, id)
			continue
		}
		hover, nameHover := m.hoverValues["node:"+string(id)], m.hoverValues["node-name:"+string(id)]
		span := m.branchSpans[id]
		if id != m.hoveredSubagent && m.hoverTarget != "node:"+string(id) && hover.value == 0 && nameHover.value == 0 && !span.running && !span.idRunning {
			delete(ids, id)
		}
	}
	p.pendingTreeRows = ids
	p.hoveredTreeID = m.hoveredSubagent
	p.hoveredBranchID = m.branchHoverNodeID()
	p.painted = nil
}
