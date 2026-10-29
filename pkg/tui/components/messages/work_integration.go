package messages

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) SetWorkProjectionEnabled(enabled bool) { _ = m.SetCompactWorkLog(enabled) }

// currentSessionOpener resolves and stores the established opener identity so
// every projection of the same transcript pins the same source row.
func (m *model) currentSessionOpener() *types.Message {
	m.sessionOpener = resolveSessionOpener(m.messages, m.sessionOpener)
	return m.sessionOpener
}

// HasWorkInCurrentExchange reports whether the latest human-input bounded
// exchange already has source-owned activity to carry its active state.
func (m *model) HasWorkInCurrentExchange() bool {
	if !m.workEnabled {
		return false
	}
	m.syncWorkProjection()
	if len(m.workProjection.exchanges) == 0 {
		return false
	}
	return len(m.workProjection.exchanges[len(m.workProjection.exchanges)-1].work) > 0
}

func (m *model) SetVisibleReplies(policy messages.VisibleReplies) tea.Cmd {
	policy = messages.ParseVisibleReplies(string(policy))
	if policy == m.visibleReplies {
		return nil
	}
	m.visibleReplies = policy
	m.suppressReplyHandoff = true
	defer func() { m.suppressReplyHandoff = false }()
	m.replyHandoff.Stop()
	m.replyHandoffFrameRows = nil
	m.replyHandoffSerial++
	m.resetPendingReplyHandoff()
	m.workProjectionDirty = true
	m.syncWorkProjection()
	cmd := tea.Batch(m.workCommands...)
	m.workCommands = nil
	m.invalidateAllItems()
	return cmd
}

func (m *model) VisibleReplies() messages.VisibleReplies { return m.visibleReplies }

// ReplyHandoffActive reports whether a finite reply fade, reflow or fade-in is running.
// Read-only; it does not change handoff state.
func (m *model) ReplyHandoffActive() bool { return m.replyHandoff.Active() }

func (m *model) SetWorkStatus(active bool, elapsed time.Duration) tea.Cmd {
	m.workActive, m.workElapsed = active, elapsed
	if !active {
		projection := projectWorkWithVisibleReplies(m.messages, m.responseGroups, m.visibleReplies, m.currentSessionOpener())
		if len(projection.exchanges) > 0 {
			exchange := projection.exchanges[len(projection.exchanges)-1]
			identity := (*types.Message)(nil)
			switch {
			case exchange.boundary >= 0 && exchange.boundary < len(m.messages):
				identity = m.messages[exchange.boundary]
			case len(exchange.work) > 0:
				identity = m.messages[exchange.work[0]]
			case len(exchange.response) > 0:
				identity = m.messages[exchange.response[0]]
			}
			if identity != nil {
				m.workDurations[identity] = elapsed
			}
		}
	}
	m.workProjectionDirty = true
	m.syncWorkProjection()
	cmd := tea.Batch(m.workCommands...)
	m.workCommands = nil
	m.invalidateAllItems()
	// A handoff in flight keeps frozen header text; repaint it with the new status.
	if m.replyHandoff.Active() {
		m.repaintHandoffHeaders()
	}
	return cmd
}

// syncSessionOpenerDefault keeps the established opener's incoming body open
// while compact Work is on. Views refuse the default once the user set their
// disclosure, and a previous opener closes when it is no longer the opener.
func (m *model) syncSessionOpenerDefault() {
	var opener *types.Message
	if m.workEnabled {
		opener = m.currentSessionOpener()
	}
	if previous := m.defaultOpenedOpener; previous != nil && previous != opener {
		m.setSessionOpenerDefault(previous, false)
	}
	if opener != nil {
		m.setSessionOpenerDefault(opener, true)
	}
	m.defaultOpenedOpener = opener
}

func (m *model) setSessionOpenerDefault(msg *types.Message, open bool) {
	index := slices.Index(m.messages, msg)
	if index < 0 || index >= len(m.views) {
		return
	}
	view, ok := m.views[index].(interface{ SetSessionOpenerDefault(open bool) bool })
	if !ok || !view.SetSessionOpenerDefault(open) {
		return
	}
	m.renderedItems.Delete(index)
	m.renderDirty = true
	m.invalidateFrameContent()
	m.invalidateView()
}

func (m *model) syncWorkProjection() {
	if !m.workEnabled {
		// Compact off only closes a previous default; this is O(1) when none exists.
		m.syncSessionOpenerDefault()
		if m.replyHandoff != nil && m.replyHandoff.Active() {
			m.replyHandoff.Stop()
			m.replyHandoffFrameRows = nil
			m.replyHandoffSerial++
		}
		for _, view := range m.views {
			view.SetSize(m.contentWidth(), 0)
		}
		for _, block := range m.workBlocks {
			block.StopAnimation()
		}
		m.workBlocks = map[*types.Message]*workblock.Model{}
		m.workProjection = workProjection{}
		m.workHeaderBySource = map[int]*workblock.Model{}
		m.workSourceToHeader = map[int]int{}
		m.workRanges = nil
		m.workProjectionDirty = false
		return
	}
	if !m.workProjectionDirty {
		return
	}
	// Resolve the opener only on dirty syncs, the same cadence as the projection.
	m.syncSessionOpenerDefault()
	m.workCommands = nil
	previous := m.workProjection
	projection := projectWorkWithVisibleReplies(m.messages, m.responseGroups, m.visibleReplies, m.currentSessionOpener())
	if m.workRetentionHeld() {
		for _, prior := range previous.exchanges {
			for _, source := range prior.response {
				if _, demoted := projection.bySource[source]; !demoted {
					continue
				}
				if source < 0 || source >= len(m.messages) {
					continue
				}
				msg := m.messages[source]
				if _, already := m.workRetained[msg]; !already {
					m.workRetained[msg] = struct{}{}
				}
			}
		}
	}
	for exchangeIndex := range projection.exchanges {
		exchange := &projection.exchanges[exchangeIndex]
		kept := exchange.work[:0]
		for _, source := range exchange.work {
			if source >= 0 && source < len(m.messages) {
				if _, retained := m.workRetained[m.messages[source]]; retained {
					exchange.response = append(exchange.response, source)
					delete(projection.bySource, source)
					continue
				}
			}
			kept = append(kept, source)
		}
		exchange.work = kept
		slices.Sort(exchange.work)
		for _, source := range exchange.work {
			projection.bySource[source] = exchangeIndex
		}
	}
	blocks := make(map[*types.Message]*workblock.Model, len(projection.exchanges))
	headerBySource := make(map[int]*workblock.Model)
	sourceToHeader := make(map[int]int)
	for exchangeIndex, exchange := range projection.exchanges {
		visibleWork := make([]int, 0, len(exchange.work))
		for _, source := range exchange.work {
			if source >= m.historyStart {
				visibleWork = append(visibleWork, source)
			}
		}
		if len(visibleWork) == 0 {
			continue
		}
		anchor := visibleWork[0]
		anchorMessage := m.messages[anchor]
		identity := anchorMessage
		if exchange.boundary >= 0 && exchange.boundary < len(m.messages) {
			identity = m.messages[exchange.boundary]
		}
		block := m.workBlocks[identity]
		if block == nil {
			for _, source := range exchange.work {
				if candidate := m.workBlocks[m.messages[source]]; candidate != nil {
					block = candidate
					break
				}
			}
		}
		if block == nil {
			block = workblock.New(m.ar)
			block.SetExpanded(m.sessionState != nil && m.sessionState.ExpandThinking())
			block.SetSize(m.contentWidth(), 0)
		}
		items := make([]workblock.Item, 0, len(visibleWork))
		for _, source := range visibleWork {
			items = append(items, workblock.Item{SourceIndex: source, View: m.views[source]})
			sourceToHeader[source] = anchor
		}
		block.SetSize(m.contentWidth(), 0)
		block.SetItems(items)
		block.SetSize(m.contentWidth(), 0)
		status := m.workStatusForIndices(visibleWork)
		status.Active = m.workActive && (exchangeIndex == len(projection.exchanges)-1)
		if status.Active {
			status.Elapsed = 0
		} else {
			status.Elapsed = m.workDurations[identity]
		}
		if cmd := block.SetStatus(status); cmd != nil {
			m.workCommands = append(m.workCommands, cmd)
		}
		blocks[identity] = block
		headerBySource[anchor] = block
	}
	for identity, old := range m.workBlocks {
		if blocks[identity] == nil {
			old.StopAnimation()
		}
	}
	m.workBlocks = blocks
	m.workHeaderBySource = headerBySource
	m.workSourceToHeader = sourceToHeader
	for source, view := range m.views {
		width := m.contentWidth()
		_, grouped := sourceToHeader[source]
		if grouped {
			width = workblock.SourceContentWidth(width)
		}
		child, ok := view.(interface{ GetSize() (int, int) })
		if !ok {
			// Views that cannot report their width are sized on every sync; SetSize is a no-op when unchanged.
			view.SetSize(width, 0)
			continue
		}
		if current, _ := child.GetSize(); current != width {
			view.SetSize(width, 0)
			if grouped {
				if owner := headerBySource[sourceToHeader[source]]; owner != nil {
					owner.InvalidateSource(source)
				}
			}
		}
	}
	m.workProjection = projection
	m.workProjectionDirty = false

	// Reply movement is owned by mutation-owned handoff scheduling.
}

func (m *model) workStatusForIndices(indices []int) workblock.Status {
	var status workblock.Status
	for _, source := range indices {
		msg := m.messages[source]
		switch msg.Type {
		case types.MessageTypeToolCall:
			if msg.ToolStatus == types.ToolStatusCompleted || msg.ToolStatus == types.ToolStatusError {
				status.ToolCount++
			}
		case types.MessageTypeAssistantReasoningBlock:
			if block, ok := m.views[source].(*reasoningblock.Model); ok {
				status.ToolCount += block.CompletedToolCount()
			}
		}
	}
	return status
}

func (m *model) workRangeAt(globalLine int) (anchor, local int, ok bool) {
	m.syncWorkProjection()
	for _, item := range m.workRanges {
		if globalLine < item.start || globalLine >= item.start+item.height {
			continue
		}
		relative := globalLine - item.start
		if source, childLine, found := item.block.SourceAt(relative); found {
			return source, childLine, true
		}
		return item.anchor, relative, true
	}
	return 0, 0, false
}

func (m *model) toggleWork(block *workblock.Model) {
	if block == nil {
		return
	}
	anchorSourceLine, anchorLocal := -1, 0
	if m.userHasScrolled && m.scrollOffset < m.totalHeight {
		if source, local, ok := m.workRangeAt(m.scrollOffset); ok {
			anchorSourceLine, anchorLocal = source, local
		}
	}
	wasAtBottom := m.isAtBottom()
	block.Toggle()
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	if anchorSourceLine >= 0 {
		if mapped := m.sourceToGlobalLine(anchorSourceLine, anchorLocal); mapped >= 0 {
			m.setScrollOffset(mapped)
		}
	} else if wasAtBottom && !m.userHasScrolled {
		m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
		m.scrollview.SetScrollOffset(m.scrollOffset)
	}
}

func (m *model) sourceToGlobalLine(source, local int) int {
	for _, block := range m.workRanges {
		if block.block == nil {
			continue
		}
		for _, row := range block.block.Rows() {
			if row.SourceIndex == source {
				return block.start + row.Start + min(local, max(0, row.End-row.Start-1))
			}
		}
		if block.anchor == source {
			return block.start
		}
		if block.block.Expanded() {
			continue
		}
		for index, item := range block.block.Items() {
			if item.SourceIndex == source {
				return block.start + 1 + index
			}
		}
	}
	if source >= 0 && source < len(m.lineOffsets) {
		return m.lineOffsets[source] + local
	}
	return -1
}

func (m *model) workItemsForAnchor(anchor int) []workblock.Item {
	if !m.workEnabled {
		return nil
	}
	for _, exchange := range m.workProjection.exchanges {
		visibleAnchor := -1
		for _, source := range exchange.work {
			if source >= m.historyStart {
				visibleAnchor = source
				break
			}
		}
		if visibleAnchor != anchor {
			continue
		}
		items := make([]workblock.Item, 0, len(exchange.work))
		for _, source := range exchange.work {
			if source >= m.historyStart && source >= 0 && source < len(m.views) {
				items = append(items, workblock.Item{SourceIndex: source, View: m.views[source]})
			}
		}
		return items
	}
	return nil
}

func (m *model) displayNeedsSeparator(index int) bool {
	if m.workEnabled {
		if _, hidden := m.workSourceToHeader[index]; hidden && m.workSourceToHeader[index] != index {
			return false
		}
	}
	if index < 0 || index >= len(m.messages)-1 {
		return false
	}
	next := index + 1
	for next < len(m.messages) && m.workEnabled {
		if _, hidden := m.workSourceToHeader[next]; !hidden || m.workSourceToHeader[next] == next {
			break
		}
		next++
	}
	if next >= len(m.messages) {
		return false
	}
	// Compact Work: a regular assistant reply reserves an invisible top action row that already separates it.
	if m.workEnabled && m.messages[next].Type == types.MessageTypeAssistant && m.actionRowEligible(next) {
		return false
	}
	return !m.compactRow(index) || !m.compactRow(next)
}

func (m *model) workCopyContent(anchor int) string {
	for _, exchange := range m.workProjection.exchanges {
		if len(exchange.work) == 0 || exchange.work[0] != anchor {
			continue
		}
		var parts []string
		for _, source := range exchange.work {
			if source < 0 || source >= len(m.messages) {
				continue
			}
			msg := m.messages[source]
			text := copyableMessageContent(msg)
			if text == "" && msg.Type == types.MessageTypeToolCall {
				text = msg.ToolCall.Function.Arguments
			}
			if strings.TrimSpace(text) != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func (m *model) WorkBlockAtSource(source int) *workblock.Model { return m.workHeaderBySource[source] }

func (m *model) copyWorkBlock(anchor int) tea.Cmd {
	text := m.workCopyContent(anchor)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return copyTextToClipboard(text)
}

func (m *model) SetCompactWorkLog(enabled bool) tea.Cmd {
	if m.workEnabled == enabled {
		return nil
	}
	wasBottom := m.isAtBottom()
	m.suppressReplyHandoff = true
	defer func() { m.suppressReplyHandoff = false }()
	if !enabled && m.replyHandoff != nil {
		m.replyHandoff.Stop()
		m.replyHandoffFrameRows = nil
		m.replyHandoffSerial++
	}
	if !wasBottom && m.scrollOffset < m.totalHeight {
		if m.workEnabled {
			if source, local, ok := m.workRangeAt(m.scrollOffset); ok {
				m.workAnchorSource, m.workAnchorLocal = source, local
			}
		} else if source, local := m.globalLineToMessageLineCached(m.scrollOffset); source >= 0 {
			m.workAnchorSource, m.workAnchorLocal = source, local
		}
	}
	m.workRetained = make(map[*types.Message]struct{})
	m.resetPendingReplyHandoff()
	m.workEnabled = enabled
	m.workProjectionDirty = true
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	if wasBottom && !m.userHasScrolled {
		m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
	} else if m.workAnchorSource >= 0 {
		if line := m.sourceToGlobalLine(m.workAnchorSource, m.workAnchorLocal); line >= 0 {
			m.scrollOffset = line
		}
	}
	m.scrollview.SetScrollOffset(m.scrollOffset)
	m.lastFrameOutput = ""
	m.visualGeneration++
	return nil
}

func (m *model) SetScrollbarPointer(x, y int) {
	if !m.workScrollbarBound {
		return
	}
	if m.scrollview.SetScrollbarPointer(x, y) {
		m.lastFrameOutput = ""
		m.visualGeneration++
	}
}

func (m *model) SetScrollbarsActive(active bool) {
	if active && !m.workScrollbarBound {
		m.scrollview.SetAnimationRuntime(m.ar)
		m.workScrollbarBound = true
	}
	if !m.workScrollbarBound {
		return
	}
	m.scrollbarsActive = active
	if m.scrollview.SetScrollbarActive(active) {
		m.lastFrameOutput = ""
		m.visualGeneration++
	}
}

func (m *model) captureVisibleReplyRows(projection workProjection) []replyHandoffLayoutRow {
	if !m.workEnabled || len(projection.exchanges) == 0 || m.renderDirty || m.replay != nil {
		return nil
	}
	// Capture the changed exchange, not only the current viewport: rows can
	// move into or out of it while reflow is in progress. Unrelated history is
	// never copied into this transient map.
	start := m.workExchangeStart(projection)
	end := m.workExchangeEnd(projection)
	if start >= end {
		return nil
	}
	rows := make([]replyHandoffLayoutRow, 0, end-start)
	for line := start; line < end; line++ {
		source, local := m.globalLineToMessageLineCanonical(line)
		row := replyHandoffLayoutRow{SourceIndex: -1, LocalRow: -1, Y: float64(line), Height: 1, CompositeLine: -1}
		inComposite := false
		for _, blockRange := range m.workRanges {
			if line < blockRange.start || line >= blockRange.start+blockRange.height {
				continue
			}
			inComposite = true
			row.Composite, row.CompositeLine, row.CompositeAnchor = blockRange.block, line-blockRange.start, blockRange.anchor
			if child, childLine, ok := blockRange.block.SourceAt(row.CompositeLine); ok {
				source, local = child, childLine
			} else {
				source, local = -1, -1
				row.SourceIndex, row.LocalRow = row.CompositeAnchor, -1
			}
			break
		}
		if source >= 0 && source < len(m.messages) {
			row.Source = m.messages[source]
			row.SourceIndex, row.LocalRow = source, local
			row.Component = m.replyComponentForSource(projection, source)
			if row.Composite == nil {
				row.Text = m.sourceTextRow(handoffFrameRow(row, 1, false))
			}
		} else if row.Composite == nil {
			row.Text = m.renderedLineCanonical(line)
		}
		if inComposite && row.CompositeLine == 0 && row.Source == nil {
			row.Text = m.sourceTextRow(handoffFrameRow(row, 1, false))
		}
		rows = append(rows, row)
	}
	return rows
}

func (m *model) workExchangeStart(projection workProjection) int {
	if len(projection.exchanges) == 0 {
		return 0
	}
	last := projection.exchanges[len(projection.exchanges)-1]
	start := int(^uint(0) >> 1)
	for _, source := range append(slices.Clone(last.work), last.response...) {
		if source >= 0 && source < len(m.lineOffsets) {
			start = min(start, m.lineOffsets[source])
		}
	}
	if start == int(^uint(0)>>1) {
		return 0
	}
	return start
}

func (m *model) workExchangeEnd(projection workProjection) int {
	if len(projection.exchanges) == 0 {
		return 0
	}
	last := projection.exchanges[len(projection.exchanges)-1]
	end := 0
	for _, source := range append(slices.Clone(last.work), last.response...) {
		if source >= 0 && source < len(m.lineOffsets) {
			rowEnd := m.displayEnd(source)
			end = max(end, rowEnd)
		}
	}
	return end
}

func (m *model) replyComponentForSource(projection workProjection, source int) replyHandoffComponentID {
	exchangeID := replyHandoffExchangeID{Generation: m.replyHandoffGeneration}
	for _, exchange := range projection.exchanges {
		if slices.Contains(exchange.work, source) || slices.Contains(exchange.response, source) {
			if exchange.boundary >= 0 && exchange.boundary < len(m.messages) {
				exchangeID.Boundary = m.messages[exchange.boundary]
			} else {
				exchangeID.Orphan = 1
			}
			break
		}
	}
	group := m.responseGroups[m.messages[source]]
	return responseHandoffComponent(exchangeID, group, m.messages[source])
}

// workRetentionHeld reports whether a live selection still pins demoted replies in the response set.
func (m *model) workRetentionHeld() bool {
	return m.selection.active || m.selectedMessageIndex >= 0
}

// snapshotPendingReplyHandoff records the reply layout before a mutation. It must
// run before content changes: a dirty render yields no rows, and a post-mutation
// recapture would show the new state as the retiring layout. placeholder is a
// prose-less reply that the mutation gives its first prose.
func (m *model) snapshotPendingReplyHandoff(placeholder *types.Message) {
	m.ensureAllItemsRendered()
	m.pendingReplyHandoffBefore = m.workProjection
	m.pendingReplyHandoffOld = m.captureVisibleReplyRows(m.workProjection)
	m.pendingReplyHandoffPlaceholder = placeholder
	m.pendingReplyHandoff = true
	// Anchor the reader on the pre-mutation rows; restore needs this for every snapshot path.
	m.captureReplyHandoffAnchor()
}

func (m *model) resetPendingReplyHandoff() {
	m.pendingReplyHandoff = false
	m.pendingReplyHandoffOld = nil
	m.pendingReplyHandoffBefore = workProjection{}
	m.pendingReplyHandoffPlaceholder = nil
}

func (m *model) beginPendingReplyHandoff() {
	if !m.pendingReplyHandoff || m.replyHandoff == nil {
		return
	}
	previous, old, placeholder := m.pendingReplyHandoffBefore, m.pendingReplyHandoffOld, m.pendingReplyHandoffPlaceholder
	m.resetPendingReplyHandoff()
	m.startReplyHandoff(previous, m.workProjection, old, placeholder)
}

func (m *model) startReplyHandoff(previous, target workProjection, oldLayout []replyHandoffLayoutRow, placeholder *types.Message) {
	if !m.workEnabled || m.suppressReplyHandoff || m.replay != nil || m.historyReplay != nil || m.historyStart > 0 || m.selection.active {
		return
	}
	targetExchange := -1
	if len(target.exchanges) > 0 {
		targetExchange = len(target.exchanges) - 1
	}
	oldExchange := -1
	if len(previous.exchanges) > 0 {
		oldExchange = len(previous.exchanges) - 1
	}
	oldResponses := make(map[replyHandoffComponentID]struct{})
	if oldExchange >= 0 {
		exchange := previous.exchanges[oldExchange]
		for _, source := range exchange.response {
			if source < 0 || source >= len(m.messages) {
				continue
			}
			oldResponses[m.replyComponentForSource(previous, source)] = struct{}{}
		}
	}
	var retiring []replyHandoffComponentID
	for id := range oldResponses {
		if !m.componentResponseVisible(target, id) {
			retiring = append(retiring, id)
		}
	}
	var incoming *replyHandoffComponentID
	if targetExchange >= 0 {
		exchange := target.exchanges[targetExchange]
		for _, source := range exchange.response {
			if source < 0 || source >= len(m.messages) {
				continue
			}
			if m.messages[source].Type != types.MessageTypeAssistant || strings.TrimSpace(m.messages[source].Content) == "" {
				continue
			}
			id := m.replyComponentForSource(target, source)
			if _, already := oldResponses[id]; !already || (placeholder != nil && m.messages[source] == placeholder) {
				incoming = &id
			}
		}
	}
	if len(retiring) == 0 && incoming == nil {
		return
	}
	oldLayout = m.filterHandoffLayoutToExchange(oldLayout, previous, oldExchange)
	targetLayout := m.filterHandoffLayoutToExchange(m.captureVisibleReplyRows(target), target, targetExchange)
	var retireCurrent []replyHandoffComponentID
	for _, row := range oldLayout {
		if slices.ContainsFunc(retiring, func(id replyHandoffComponentID) bool { return id == row.Component }) {
			retireCurrent = append(retireCurrent, row.Component)
		}
	}
	if m.replyHandoff.Active() {
		m.replyHandoff.Begin(oldLayout, targetLayout, retireCurrent, incoming)
		return
	}
	if len(retireCurrent) > 0 || incoming != nil {
		m.replyHandoff.Begin(oldLayout, targetLayout, retireCurrent, incoming)
		m.refreshReplyHandoffRows()
		m.restoreReplyHandoffAnchor(m.replyHandoffFrameRows)
		m.replyHandoffSerial++
		m.invalidateFrameContent()
		if len(retireCurrent) > 0 {
			m.clearPointerHoverTargets()
		}
	}
}

func (m *model) filterHandoffLayoutToExchange(layout []replyHandoffLayoutRow, projection workProjection, exchangeIndex int) []replyHandoffLayoutRow {
	if exchangeIndex < 0 || exchangeIndex >= len(projection.exchanges) {
		return nil
	}
	exchange := projection.exchanges[exchangeIndex]
	allowed := make(map[int]struct{}, len(exchange.work)+len(exchange.response))
	for _, source := range exchange.work {
		allowed[source] = struct{}{}
	}
	for _, source := range exchange.response {
		allowed[source] = struct{}{}
	}
	filtered := make([]replyHandoffLayoutRow, 0, len(layout))
	for _, row := range layout {
		if row.Source != nil {
			if _, ok := allowed[row.SourceIndex]; ok {
				filtered = append(filtered, row)
			}
			continue
		}
		if row.Composite != nil && slices.Contains(exchange.work, row.CompositeAnchor) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (m *model) componentResponseVisible(projection workProjection, component replyHandoffComponentID) bool {
	for _, exchange := range projection.exchanges {
		for _, source := range exchange.response {
			if m.replyComponentForSource(projection, source) == component {
				return true
			}
		}
	}
	return false
}

func (m *model) captureReplyHandoffAnchor() {
	clear(m.replyHandoffAnchors)
	if !m.userHasScrolled || m.height <= 0 {
		return
	}
	for screenY := range m.height {
		line := m.scrollOffset + screenY
		source, local := m.globalLineToMessageLineCanonical(line)
		if source < 0 || source >= len(m.messages) {
			continue
		}
		if _, exists := m.replyHandoffAnchors[m.messages[source]]; !exists {
			m.replyHandoffAnchors[m.messages[source]] = replyHandoffAnchor{localRow: local, screenY: screenY}
		}
	}
}

func (m *model) rebaseReplyHandoffAfterResize() {
	if m.replyHandoff == nil || !m.replyHandoff.Active() || len(m.workProjection.exchanges) == 0 {
		return
	}
	target := m.captureVisibleReplyRows(m.workProjection)
	target = m.filterHandoffLayoutToExchange(target, m.workProjection, len(m.workProjection.exchanges)-1)
	oldLayout := m.replyHandoff.oldLayout
	for i := range oldLayout {
		if oldLayout[i].Source == nil || oldLayout[i].LocalRow < 0 {
			continue
		}
		index := m.messagePointerIndex(oldLayout[i].Source)
		if index < 0 || index >= len(m.views) {
			continue
		}
		item := m.renderItem(index, m.views[index])
		lines := m.renderedItemLines(item)
		if oldLayout[i].LocalRow < len(lines) {
			oldLayout[i].Text = lines[oldLayout[i].LocalRow]
		}
		oldLayout[i].SourceIndex = index
	}
	m.replyHandoff.Rebase(oldLayout, target)
	m.refreshReplyHandoffRows()
	m.replyHandoffSerial++
	m.invalidateFrameContent()
}

func (m *model) refreshReplyHandoffRows() {
	rows := m.replyHandoff.Rows()
	for i := range rows {
		if rows[i].Source != nil {
			rows[i].SourceIndex = m.messagePointerIndex(rows[i].Source)
		}
	}
	m.replyHandoffFrameRows = rows
}

// handoffSyncMemo caches one sync pass's rendered sources and split composites.
// It is local to a single syncAnimatedWorkProjection call; nothing survives the pass.
type handoffSyncMemo struct {
	items      map[int]renderedItem
	composites map[*workblock.Model][]string
}

// repaintHandoffHeaders updates only work-block header text in the frame rows on
// screen, so handoff layout, scroll anchors and hover stay untouched.
func (m *model) repaintHandoffHeaders() {
	var rows []replyHandoffFrameRow
	for i, row := range m.replyHandoffFrameRows {
		if row.Composite == nil || row.Source != nil || row.CompositeLine != 0 {
			continue
		}
		lines := m.compositeLines(row.Composite, nil)
		if len(lines) == 0 || row.Text == lines[0] {
			continue
		}
		if rows == nil {
			rows = slices.Clone(m.replyHandoffFrameRows)
		}
		rows[i].Text = lines[0]
	}
	if rows == nil {
		return
	}
	m.replyHandoffFrameRows = rows
	m.replyHandoffSerial++
	m.invalidateFrameContent()
}

func (m *model) syncAnimatedWorkProjection() {
	rows := m.replyHandoff.Rows()
	memo := handoffSyncMemo{items: make(map[int]renderedItem), composites: make(map[*workblock.Model][]string)}
	for i := range rows {
		if rows[i].Source != nil {
			rows[i].SourceIndex = m.messagePointerIndex(rows[i].Source)
		}
		// Composite header rows have no source; refresh them too or a status change keeps frozen text.
		if rows[i].Source != nil || rows[i].Composite != nil {
			rows[i].Text = m.sourceTextRowMemo(rows[i], &memo)
		}
	}
	if len(rows) != len(m.replyHandoffFrameRows) || !slices.Equal(rows, m.replyHandoffFrameRows) {
		previous := m.replyHandoffFrameRows
		m.replyHandoffFrameRows = rows
		m.replyHandoffSerial++
		m.invalidateFrameContent()
		if !sameHandoffPointerGeometry(rows, previous) {
			m.clearPointerHoverTargets()
		}
		m.restoreReplyHandoffAnchor(rows)
		m.refreshPointerHover()
	}
}

func (m *model) restoreReplyHandoffAnchor(rows []replyHandoffFrameRow) {
	if !m.userHasScrolled {
		return
	}
	for _, row := range rows {
		if row.Source == nil || !row.Interactive {
			continue
		}
		if anchor, ok := m.replyHandoffAnchors[row.Source]; ok && row.LocalRow == anchor.localRow {
			first, _ := replyHandoffTerminalSpan(row.Y, row.Height)
			m.scrollOffset = max(0, first-anchor.screenY)
			m.scrollview.SetScrollOffset(m.scrollOffset)
			return
		}
	}
}

func sameHandoffPointerGeometry(a, b []replyHandoffFrameRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Source != b[i].Source || a[i].SourceIndex != b[i].SourceIndex || a[i].LocalRow != b[i].LocalRow ||
			a[i].Y != b[i].Y || a[i].Height != b[i].Height || a[i].Spacer != b[i].Spacer || a[i].Interactive != b[i].Interactive {
			return false
		}
	}
	return true
}

func (m *model) applyReplyHandoffPaint() {
	m.replyHandoffFrameLines = nil
	if m.replyHandoff == nil || !m.replyHandoff.Active() {
		return
	}
	rows := m.replyHandoffFrameRows
	if len(rows) == 0 {
		return
	}
	start := m.scrollOffset
	end := min(m.scrollOffset+m.height, m.displayTotalHeight()+m.bottomSlack)
	fadeContext := styles.NewFadeContext()
	lines := make(map[int]string, end-start)
	for _, layout := range [][]replyHandoffLayoutRow{m.replyHandoff.oldLayout, m.replyHandoff.targetLayout} {
		for _, row := range layout {
			first, last := replyHandoffTerminalSpan(row.Y, row.Height)
			for y := max(first, start); y < min(last, end); y++ {
				lines[y] = ""
			}
		}
	}
	for _, row := range rows {
		first, last := replyHandoffTerminalSpan(row.Y, row.Height)
		if last <= start || first >= end {
			continue
		}
		if row.Spacer {
			for y := max(first, start); y < min(last, end); y++ {
				lines[y] = ""
			}
			continue
		}
		if row.Height <= 0 || row.Text == "" {
			continue
		}
		text := row.Text
		if row.Opacity < 1 {
			text = styles.FadeLineCtx(text, row.Opacity, &fadeContext)
		}
		for y := max(first, start); y < min(last, end); y++ {
			lines[y] = text
		}
	}
	m.replyHandoffFrameLines = lines
}

func (m *model) sourceTextRow(row replyHandoffFrameRow) string {
	return m.sourceTextRowMemo(row, nil)
}

// compositeLines splits a work block once per pass when a memo is supplied.
func (m *model) compositeLines(block *workblock.Model, memo *handoffSyncMemo) []string {
	if memo != nil {
		if lines, ok := memo.composites[block]; ok {
			return lines
		}
	}
	lines := strings.Split(strings.TrimSuffix(block.View(), "\n"), "\n")
	if memo != nil {
		memo.composites[block] = lines
	}
	return lines
}

// renderItemMemo renders a source once per pass when a memo is supplied.
func (m *model) renderItemMemo(index int, memo *handoffSyncMemo) renderedItem {
	if memo != nil {
		if item, ok := memo.items[index]; ok {
			return item
		}
	}
	item := m.renderItem(index, m.views[index])
	if memo != nil {
		memo.items[index] = item
	}
	return item
}

func (m *model) sourceTextRowMemo(row replyHandoffFrameRow, memo *handoffSyncMemo) string {
	if row.Composite != nil && row.CompositeLine >= 0 {
		lines := m.compositeLines(row.Composite, memo)
		if row.CompositeLine < len(lines) {
			return lines[row.CompositeLine]
		}
		return ""
	}
	source := row.Source
	localRow := row.LocalRow
	if source == nil {
		return ""
	}
	index := m.messagePointerIndex(source)
	if index < 0 {
		return ""
	}
	if index == len(m.views)-1 && m.activeSegments != nil && m.activeSegments.index == index {
		if localRow >= 0 && localRow < m.activeSegments.height() {
			return m.activeSegments.line(localRow)
		}
		return ""
	}
	item := m.renderItemMemo(index, memo)
	if localRow < 0 || localRow >= item.height {
		return ""
	}
	if item.segments != nil {
		if localRow < len(item.segments.Header) {
			return item.segments.Header[localRow]
		}
		localRow -= len(item.segments.Header)
		if localRow < len(item.segments.Stable) {
			return item.segments.Stable[localRow]
		}
		localRow -= len(item.segments.Stable)
		if localRow >= 0 && localRow < len(item.segments.Tail) {
			return item.segments.Tail[localRow]
		}
		return ""
	}
	if localRow < len(item.lines) {
		return item.lines[localRow]
	}
	return ""
}

func (m *model) displayTotalHeight() int {
	// Replay owns totalHeight directly; handoff rows never describe a replay frame.
	if m.replay != nil || m.replyHandoff == nil || !m.replyHandoff.Active() || len(m.replyHandoffFrameRows) == 0 {
		return m.totalHeight
	}
	total := 0.0
	for _, row := range m.replyHandoffFrameRows {
		_, last := replyHandoffTerminalSpan(row.Y, row.Height)
		total = maxFloat(total, float64(last))
	}
	return max(m.totalHeight, int(total))
}

func (m *model) replyHandoffBounds() (int, int) {
	if m.replyHandoff == nil || !m.replyHandoff.Active() {
		return 0, 0
	}
	start := int(^uint(0) >> 1)
	end := 0
	for _, layout := range [][]replyHandoffLayoutRow{m.replyHandoff.oldLayout, m.replyHandoff.targetLayout} {
		for _, row := range layout {
			first, last := replyHandoffTerminalSpan(row.Y, row.Height)
			start = min(start, first)
			end = max(end, last)
		}
	}
	if start == int(^uint(0)>>1) {
		return 0, 0
	}
	return start, end
}

// replyHandoffTerminalSpan is the single cell quantization used by handoff
// paint, hits, bounds, and reader anchors; fractional motion never splits maps.
func replyHandoffTerminalSpan(y, height float64) (first, last int) {
	if height <= 0 {
		return 0, 0
	}
	first = int(y + 0.5)
	last = int(y + height)
	if float64(last) < y+height {
		last++
	}
	if last <= first {
		last = first + 1
	}
	return first, last
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// replyHandoffRowAt uses the same last-painted-wins order as the renderer
// when fractional rows quantize onto the same terminal cell.
func (m *model) replyHandoffRowAt(globalLine int) (replyHandoffFrameRow, bool) {
	for _, row := range slices.Backward(m.replyHandoffFrameRows) {
		first, last := replyHandoffTerminalSpan(row.Y, row.Height)
		if row.Height > 0 && globalLine >= first && globalLine < last {
			return row, true
		}
	}
	return replyHandoffFrameRow{}, false
}

func (m *model) workBlockLineAt(globalLine, source int) int {
	if m.replyHandoff != nil && m.replyHandoff.Active() {
		if row, ok := m.replyHandoffRowAt(globalLine); ok && row.Composite != nil && row.SourceIndex == source {
			return row.CompositeLine
		}
	}
	return globalLine - m.workRangeStart(source)
}

func (m *model) workBlockForSource(source int) *workblock.Model {
	anchor, ok := m.workSourceToHeader[source]
	if !ok {
		return nil
	}
	return m.workHeaderBySource[anchor]
}

func (m *model) releaseDeferredWorkFold() {
	m.releaseRetainedReplies()
}

func (m *model) workSourceForBlock(block *workblock.Model) int {
	for source, candidate := range m.workHeaderBySource {
		if candidate == block {
			return source
		}
	}
	return -1
}

func (m *model) workRangeStart(source int) int {
	anchor, ok := m.workSourceToHeader[source]
	if !ok {
		return 0
	}
	for _, item := range m.workRanges {
		if item.anchor == anchor {
			return item.start
		}
	}
	return 0
}

func (m *model) refreshWorkStatusForSource(source int) {
	block := m.workBlockForSource(source)
	if block == nil {
		return
	}
	// A pending rebuild recomputes statuses from current message indices; the
	// stale projection may still reference indices that a truncation removed.
	if m.workProjectionDirty {
		return
	}
	anchor := m.workSourceToHeader[source]
	for i, exchange := range m.workProjection.exchanges {
		if len(exchange.work) == 0 || exchange.work[0] != anchor {
			continue
		}
		status := m.workStatusForIndices(exchange.work)
		status.Active = m.workActive && i == len(m.workProjection.exchanges)-1
		switch {
		case status.Active:
			status.Elapsed = 0
		case exchange.boundary >= 0 && exchange.boundary < len(m.messages):
			status.Elapsed = m.workDurations[m.messages[exchange.boundary]]
		default:
			status.Elapsed = 0
		}
		if cmd := block.SetStatus(status); cmd != nil {
			m.workCommands = append(m.workCommands, cmd)
		}
		return
	}
}

func (m *model) visibleCandidateLines(source int) []string {
	if source < 0 || source >= len(m.views) || source >= len(m.lineOffsets) {
		return nil
	}
	start := m.lineOffsets[source]
	item := m.renderItem(source, m.views[source])
	lines := m.renderedItemLines(item)
	if len(lines) == 0 {
		return nil
	}
	visibleStart := max(start, m.scrollOffset)
	visibleEnd := min(start+len(lines), m.scrollOffset+m.height)
	if visibleEnd <= visibleStart {
		return nil
	}
	lo, hi := visibleStart-start, visibleEnd-start
	return append([]string(nil), lines[lo:hi]...)
}

func (m *model) invalidateWorkBlock(block *workblock.Model) {
	if source := m.workSourceForBlock(block); source >= 0 {
		m.renderedItems.Delete(source)
		m.renderDirty = true
		m.invalidateView()
	}
}

// updateReplyHandoffTarget refreshes only the active exchange's current visible
// rows after authoritative source ingestion; it never restarts the phase clock.
func (m *model) updateReplyHandoffTarget() {
	if m.replyHandoff == nil || !m.replyHandoff.Active() {
		return
	}
	components := m.replyHandoff.batch.incomingComponents()
	if m.replyHandoff.queued != nil {
		components = uniqueComponents(append(components, m.replyHandoff.queued.incomingComponents()...))
	}
	for _, incoming := range components {
		var sourceIndexes []int
		appendLayoutSources := func(layout []replyHandoffLayoutRow) {
			for _, row := range layout {
				if row.Component == incoming && row.Source != nil && row.SourceIndex >= 0 && !slices.Contains(sourceIndexes, row.SourceIndex) {
					sourceIndexes = append(sourceIndexes, row.SourceIndex)
				}
			}
		}
		appendLayoutSources(m.replyHandoff.targetLayout)
		appendLayoutSources(m.replyHandoff.queuedLayout)
		slices.Sort(sourceIndexes)
		var rows []replyHandoffLayoutRow
		for _, i := range sourceIndexes {
			if i < 0 || i >= len(m.messages) || i >= len(m.views) || responseHandoffComponent(incoming.Exchange, m.responseGroups[m.messages[i]], m.messages[i]) != incoming {
				continue
			}
			msg, ok := m.views[i].(message.Model)
			if !ok {
				continue
			}
			item := m.renderItem(i, msg)
			lines := item.lines
			if item.segments != nil {
				lines = make([]string, 0, item.height)
				lines = append(lines, item.segments.Header...)
				lines = append(lines, item.segments.Stable...)
				lines = append(lines, item.segments.Tail...)
			}
			start := 0
			if i < len(m.lineOffsets) {
				start = m.lineOffsets[i]
			}
			for local, text := range lines {
				rows = append(rows, replyHandoffLayoutRow{
					Source: m.messages[i], SourceIndex: i, LocalRow: local, Component: incoming, Text: text,
					Y: float64(start + local), Height: 1, CompositeLine: -1,
				})
			}
		}
		if m.replyHandoff.batch.isIncoming(incoming) {
			m.replyHandoff.UpdateComponent(incoming, rows)
		} else {
			m.replyHandoff.UpdateQueuedComponent(incoming, rows)
		}
	}
	m.refreshReplyHandoffRows()
	m.replyHandoffSerial++
	m.invalidateFrameContent()
}
