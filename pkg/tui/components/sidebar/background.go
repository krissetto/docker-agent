package sidebar

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	backgroundInitialRows = 5
	backgroundGrace       = 4 * time.Second
)

type backgroundRowCacheEntry struct {
	process tools.BackgroundProcess
	width   int
	elapsed string
	spinner string
	hover   float64
	text    string
}

func (m *model) resetBackgroundProcesses(sessionID string) {
	sameSession := m.backgroundSessionID == sessionID
	manual, collapsed, showAll := m.backgroundManual, m.backgroundCollapsed, m.backgroundShowAll
	m.backgroundSessionID = sessionID
	m.backgroundProcesses = nil
	m.backgroundProcessesByID = nil
	m.backgroundExpiry = nil
	m.backgroundNextExpiry = time.Time{}
	m.backgroundRunningCount = 0
	m.backgroundRowCache = nil
	m.backgroundRows = nil
	m.backgroundLines = nil
	m.backgroundRowIDs = nil
	m.backgroundRowTools = nil
	m.backgroundMoreOffset = -1
	m.backgroundManual = sameSession && manual
	m.backgroundCollapsed = sameSession && manual && collapsed
	m.backgroundShowAll = sameSession && manual && showAll
	m.backgroundDisclosureRunning = false
	m.backgroundDisclosureAlpha = 0
	m.backgroundDisclosureTarget = 0
	m.backgroundAnimation.Stop()
}

// SetBackgroundProcesses replaces the adopted, session-scoped process snapshot.
// Process IDs are presentation identities only; navigation uses ToolCallID.
func (m *model) SetBackgroundProcesses(sessionID string, processes []tools.BackgroundProcess) tea.Cmd {
	if sessionID == "" || m.activeBackgroundSession() != "" && sessionID != m.activeBackgroundSession() {
		return nil
	}
	now := m.backgroundNow()
	if m.backgroundSessionID == sessionID && m.sameBackgroundSnapshot(processes, now) {
		return nil
	}
	// Expired terminal rows are not resurrected by a repeat snapshot.
	filtered := make([]tools.BackgroundProcess, 0, len(processes))
	expiry := make(map[string]time.Time, len(processes))
	var nextExpiry time.Time
	runningCount := 0
	seenIDs := make(map[string]struct{}, len(processes))
	for _, process := range processes {
		if process.ID == "" || process.SessionID != sessionID {
			continue
		}
		if _, duplicate := seenIDs[process.ID]; duplicate {
			continue
		}
		seenIDs[process.ID] = struct{}{}
		if process.State == tools.BackgroundProcessRunning {
			runningCount++
		} else {
			deadline := process.FinishedAt
			if deadline.IsZero() {
				deadline = now
				if m.backgroundSessionID == sessionID {
					if prior, ok := m.backgroundExpiry[process.ID]; ok {
						deadline = prior.Add(-backgroundGrace)
					}
				}
			}
			deadline = deadline.Add(backgroundGrace)
			if !now.Before(deadline) {
				continue
			}
			expiry[process.ID] = deadline
			if nextExpiry.IsZero() || deadline.Before(nextExpiry) {
				nextExpiry = deadline
			}
		}
		filtered = append(filtered, process)
	}
	slices.SortFunc(filtered, func(a, b tools.BackgroundProcess) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	if m.backgroundSessionID == sessionID && sameBackgroundProcesses(m.backgroundProcesses, filtered) {
		return nil
	}
	oldSession := m.backgroundSessionID
	m.backgroundSessionID = sessionID
	m.backgroundProcesses = filtered
	m.backgroundProcessesByID = make(map[string]tools.BackgroundProcess, len(filtered))
	for _, process := range filtered {
		m.backgroundProcessesByID[process.ID] = process
	}
	m.backgroundExpiry = expiry
	m.backgroundNextExpiry = nextExpiry
	m.backgroundRunningCount = runningCount
	m.backgroundRowCache = nil
	m.backgroundRows = nil
	m.backgroundRowIDs = nil
	m.backgroundRowTools = nil
	if oldSession != sessionID {
		m.backgroundManual = false
		m.backgroundShowAll = false
		m.backgroundCollapsed = len(m.sessionStack) > 0
	}
	if len(filtered) == 0 {
		m.backgroundAnimation.Stop()
		m.backgroundDisclosureRunning = false
		m.backgroundDisclosureAlpha = 0
		m.backgroundDisclosureTarget = 0
		m.backgroundExpiry = nil
	} else if !m.backgroundManual {
		m.backgroundCollapsed = len(m.sessionStack) > 0
	}
	m.backgroundFrame = ""
	m.invalidateBackgroundStructure()
	return tea.Batch(m.ReconcileLayout(), m.syncBackgroundAnimation())
}

func (m *model) sameBackgroundSnapshot(processes []tools.BackgroundProcess, now time.Time) bool {
	if len(processes) != len(m.backgroundProcessesByID) {
		return false
	}
	for i, process := range processes {
		if process.ID == "" || process.SessionID != m.backgroundSessionID {
			return false
		}
		if i > 0 {
			previous := processes[i-1]
			if previous.ID == process.ID {
				return false
			}
			if order := previous.StartedAt.Compare(process.StartedAt); order > 0 || order == 0 && previous.ID > process.ID {
				return false
			}
		}
		stored, ok := m.backgroundProcessesByID[process.ID]
		if !ok || process != stored {
			return false
		}
		if process.State != tools.BackgroundProcessRunning {
			deadline := process.FinishedAt
			if deadline.IsZero() {
				if prior, found := m.backgroundExpiry[process.ID]; found {
					deadline = prior.Add(-backgroundGrace)
				} else {
					deadline = now
				}
			}
			if !now.Before(deadline.Add(backgroundGrace)) {
				return false
			}
		}
	}
	return true
}

func sameBackgroundProcesses(a, b []tools.BackgroundProcess) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *model) activeBackgroundSession() string {
	if m.rootSessionID != "" {
		return m.rootSessionID
	}
	return m.activeSessionID()
}

func (m *model) backgroundNow() time.Time {
	if m.backgroundNowFunc != nil {
		return m.backgroundNowFunc()
	}
	return time.Now()
}

func (m *model) syncBackgroundDefaultOpen() {
	if len(m.backgroundProcesses) == 0 || m.backgroundManual {
		return
	}
	m.backgroundCollapsed = len(m.sessionStack) > 0
}

func (m *model) backgroundRunning() bool { return m.backgroundRunningCount > 0 }

func (m *model) backgroundAnimationNeeded() bool {
	if !m.presentationActive || len(m.backgroundProcesses) == 0 {
		return false
	}
	if m.mode == ModeCollapsed {
		return m.backgroundRunning() || len(m.backgroundExpiry) > 0
	}
	if m.mode != ModeVertical {
		return false
	}
	visible := m.backgroundStart >= 0 && m.backgroundEnd > m.scrollview.ScrollOffset() && m.backgroundStart < m.scrollview.ScrollOffset()+m.viewportHeight()
	return visible && (m.backgroundRunning() || len(m.backgroundExpiry) > 0 || m.backgroundDisclosureRunning)
}

func (m *model) syncBackgroundAnimation() tea.Cmd {
	if m.backgroundAnimationNeeded() {
		return m.backgroundAnimation.Start()
	}
	m.backgroundAnimation.Stop()
	return nil
}

func (m *model) resumeBackgroundAnimation() tea.Cmd {
	if len(m.backgroundExpiry) > 0 && !m.backgroundNow().Before(m.backgroundNextExpiry) {
		now := m.backgroundNow()
		filtered := m.backgroundProcesses[:0]
		for _, process := range m.backgroundProcesses {
			if deadline, ok := m.backgroundExpiry[process.ID]; ok && !now.Before(deadline) {
				delete(m.backgroundExpiry, process.ID)
				continue
			}
			filtered = append(filtered, process)
		}
		if len(filtered) != len(m.backgroundProcesses) {
			m.backgroundProcesses = filtered
			m.backgroundProcessesByID = make(map[string]tools.BackgroundProcess, len(filtered))
			for _, process := range filtered {
				m.backgroundProcessesByID[process.ID] = process
			}
			m.backgroundRunningCount = 0
			for _, process := range filtered {
				if process.State == tools.BackgroundProcessRunning {
					m.backgroundRunningCount++
				}
			}
			m.backgroundRowCache = nil
			m.invalidateCache()
		}
		m.backgroundNextExpiry = time.Time{}
		for _, deadline := range m.backgroundExpiry {
			if m.backgroundNextExpiry.IsZero() || deadline.Before(m.backgroundNextExpiry) {
				m.backgroundNextExpiry = deadline
			}
		}
	}
	return m.syncBackgroundAnimation()
}

func (m *model) tickBackgroundProcesses(tick animation.TickMsg) {
	if !m.backgroundAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	frameChanged := m.backgroundRunning() && animation.Chat.FrameAt(before) != animation.Chat.FrameAt(after)
	disclosureChanged := false
	if m.backgroundDisclosureRunning {
		old := m.backgroundDisclosureAlpha
		m.backgroundDisclosureAlpha = animation.HoverStep(old, m.backgroundDisclosureTarget, after-before)
		disclosureChanged = old != m.backgroundDisclosureAlpha
		if m.backgroundDisclosureAlpha == m.backgroundDisclosureTarget {
			m.backgroundDisclosureRunning = false
			if m.backgroundDisclosureTarget == 0 {
				m.backgroundCollapsed = true
				m.backgroundShowAll = false
				m.invalidateBackgroundStructure()
			} else {
				m.backgroundDisclosureFrom = -1
			}
		}
	}
	if frameChanged {
		m.backgroundFrame = animation.Chat.FrameAt(after)
	}
	now := m.backgroundNow()
	expired := false
	if len(m.backgroundExpiry) > 0 && !now.Before(m.backgroundNextExpiry) {
		m.backgroundNextExpiry = time.Time{}
		for i := range m.backgroundProcesses {
			process := m.backgroundProcesses[i]
			if deadline, ok := m.backgroundExpiry[process.ID]; ok && !now.Before(deadline) {
				delete(m.backgroundExpiry, process.ID)
				m.backgroundProcesses[i].ID = ""
				expired = true
			}
		}
		if expired {
			m.backgroundProcesses = slices.DeleteFunc(m.backgroundProcesses, func(process tools.BackgroundProcess) bool { return process.ID == "" })
			m.backgroundProcessesByID = make(map[string]tools.BackgroundProcess, len(m.backgroundProcesses))
			m.backgroundRunningCount = 0
			for _, process := range m.backgroundProcesses {
				m.backgroundProcessesByID[process.ID] = process
				if process.State == tools.BackgroundProcessRunning {
					m.backgroundRunningCount++
				}
			}
			m.backgroundRowCache = nil
		}
		for _, deadline := range m.backgroundExpiry {
			if m.backgroundNextExpiry.IsZero() || deadline.Before(m.backgroundNextExpiry) {
				m.backgroundNextExpiry = deadline
			}
		}
	}
	elapsedChanged := m.refreshVisibleBackgroundElapsed(now)
	if expired {
		m.invalidateBackgroundStructure()
		m.syncBackgroundAnimation()
		tick.MarkDirty()
		return
	}
	if frameChanged || elapsedChanged || disclosureChanged {
		if m.mode == ModeVertical {
			m.refreshBackgroundMotion()
		}
		m.visualGeneration++
		tick.MarkDirty()
		return
	}
}

// Only rows in the viewport need elapsed-time refresh. Offscreen rows acquire
// current text when they enter view, without a per-process timer or tick work.
func (m *model) refreshVisibleBackgroundElapsed(now time.Time) bool {
	if m.backgroundStart < 0 || len(m.backgroundProcesses) == 0 || m.backgroundCollapsed {
		return false
	}
	first := max(0, m.scrollview.ScrollOffset()-m.backgroundStart-1)
	last := min(len(m.backgroundProcesses), m.scrollview.ScrollOffset()+m.viewportHeight()-m.backgroundStart-1)
	if !m.backgroundShowAll {
		last = min(last, backgroundInitialRows)
	}
	changed := false
	for index := first; index < last; index++ {
		process := m.backgroundProcesses[index]
		if process.State != tools.BackgroundProcessRunning {
			continue
		}
		entry, ok := m.backgroundRowCache[process.ID]
		if !ok {
			continue
		}
		elapsed := formatBackgroundElapsed(now.Sub(process.StartedAt))
		if elapsed != entry.elapsed {
			entry.elapsed = elapsed
			entry.text = ""
			m.backgroundRowCache[process.ID] = entry
			changed = true
		}
	}
	return changed
}

func formatBackgroundElapsed(elapsed time.Duration) string {
	seconds := max(0, int(elapsed.Seconds()))
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
}

func (m *model) backgroundHeaderLine(width int) string {
	glyph := "›"
	if !m.backgroundCollapsed || m.backgroundDisclosureRunning && m.backgroundDisclosureTarget > 0 {
		glyph = "⌄"
	}
	title := styles.TabPrimaryStyle.Render(fmt.Sprintf("Background · %d processes", len(m.backgroundProcesses)))
	if m.backgroundRunning() {
		frame := m.backgroundFrame
		if frame == "" {
			frame = animation.Chat.FrameAt(m.ar.Now())
		}
		title = styles.TabPrimaryStyle.Render("Background") + " · " + styles.SpinnerDotsHighlightStyle.Render(frame) + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d processes", len(m.backgroundProcesses)))
	}
	if width > 1 {
		short := ansi.Truncate(title, width-1, "…")
		title = short + strings.Repeat(" ", max(0, width-1-ansi.StringWidth(short))) + styles.MutedStyle.Render(glyph)
	} else {
		title = ansi.Truncate(title, width, "")
	}
	return m.hoverText(title, "background:header")
}

func (m *model) backgroundInfo(width int) string {
	if len(m.backgroundProcesses) == 0 || width <= 0 {
		return ""
	}
	lines := m.backgroundLines[:0]
	if cap(lines) < min(len(m.backgroundProcesses), backgroundInitialRows)+2 {
		lines = make([]string, 0, min(len(m.backgroundProcesses), backgroundInitialRows)+2)
	}
	count := len(m.backgroundProcesses)
	title := m.backgroundHeaderLine(width)
	lines = append(lines, title)
	m.backgroundRows = m.backgroundRows[:0]
	m.backgroundRowIDs = m.backgroundRowIDs[:0]
	m.backgroundRowTools = m.backgroundRowTools[:0]
	m.backgroundMoreOffset = -1
	showRows := !m.backgroundCollapsed || m.backgroundDisclosureRunning && m.backgroundDisclosureTarget == 0
	if showRows {
		limit := count
		if !m.backgroundShowAll {
			limit = min(limit, backgroundInitialRows)
		}
		for i := range limit {
			process := m.backgroundProcesses[i]
			line := m.backgroundRow(process, width, m.backgroundNow(), i)
			if m.backgroundDisclosureRunning && i >= m.backgroundDisclosureFrom {
				line = styles.FadeLine(line, m.backgroundDisclosureAlpha)
			}
			lines = append(lines, line)
			m.backgroundRows = append(m.backgroundRows, line)
			m.backgroundRowIDs = append(m.backgroundRowIDs, process.ID)
			m.backgroundRowTools = append(m.backgroundRowTools, process.ToolCallID)
		}
		if limit < count {
			m.backgroundMoreOffset = len(lines)
			more := styles.MutedStyle.Render(fmt.Sprintf("  … +%d more", count-limit))
			if m.backgroundDisclosureRunning {
				more = styles.FadeLine(more, m.backgroundDisclosureAlpha)
			}
			lines = append(lines, more)
		}
	}
	m.backgroundLines = lines
	return strings.Join(lines, "\n")
}

func (m *model) backgroundRowVisible(index int) bool {
	if m.backgroundStart < 0 {
		return true
	}
	y := m.backgroundStart + 1 + index
	return y >= m.scrollview.ScrollOffset() && y < m.scrollview.ScrollOffset()+m.viewportHeight()
}

func sanitizeBackgroundCommand(command string) string {
	if newline := strings.IndexAny(command, "\r\n"); newline >= 0 {
		command = command[:newline]
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, command)
}

func (m *model) backgroundRow(process tools.BackgroundProcess, width int, now time.Time, index int) string {
	if m.backgroundRowCache == nil {
		m.backgroundRowCache = make(map[string]backgroundRowCacheEntry)
	}
	entry := m.backgroundRowCache[process.ID]
	visible := m.backgroundRowVisible(index)
	frame := ""
	if process.State == tools.BackgroundProcessRunning {
		if !visible && entry.process == process && entry.width == width {
			frame = entry.spinner
		}
		if frame == "" {
			frame = m.backgroundFrame
		}
		if frame == "" {
			frame = animation.Chat.FrameAt(m.ar.Now())
		}
	}
	elapsed := ""
	if process.State == tools.BackgroundProcessRunning {
		elapsed = formatBackgroundElapsed(now.Sub(process.StartedAt))
		if !visible && entry.process == process && entry.width == width {
			elapsed = entry.elapsed
		}
	}
	hover := m.hoverValues["background:"+process.ID].value
	if entry.process == process && entry.width == width && entry.elapsed == elapsed && entry.spinner == frame && entry.hover == hover && entry.text != "" {
		return entry.text
	}
	entry = backgroundRowCacheEntry{process: process, width: width, elapsed: elapsed, spinner: frame, hover: hover}
	command := sanitizeBackgroundCommand(process.Command)
	command = ansi.Truncate(command, max(1, width-12), "…")
	glyph, style := "•", styles.MutedStyle
	switch process.State {
	case tools.BackgroundProcessRunning:
		glyph, style = frame, styles.SpinnerDotsHighlightStyle
	case tools.BackgroundProcessCompleted:
		glyph, style = "✓", styles.CompletedStyle
	case tools.BackgroundProcessStopped:
		glyph, style = "■", styles.MutedStyle
	case tools.BackgroundProcessFailed:
		glyph, style = "✗", styles.ErrorStyle
	}
	prefix := "  " + style.Render(glyph) + " "
	commandText := styles.BaseStyle.Foreground(styles.TextPrimary).Render(command)
	if process.AgentName != "" {
		commandText = styles.AgentIdentityStyle(process.AgentName, false).Render(command)
	}
	body := prefix + commandText
	if elapsed != "" {
		body += strings.Repeat(" ", max(1, width-ansi.StringWidth(body)-ansi.StringWidth(elapsed))) + styles.MutedStyle.Render(elapsed)
	}
	body = ansi.Truncate(body, width, "…")
	if hover > 0 {
		body = styles.HoverText(body, hover, styles.TextPrimary)
	}
	entry.text = body
	m.backgroundRowCache[process.ID] = entry
	m.backgroundRowRenders++
	return body
}

func (m *model) registerBackgroundGeometry(start int) {
	m.backgroundStart = start
	m.backgroundHeader = start
	m.backgroundEnd = start + 1 + len(m.backgroundRows)
	m.backgroundMoreLine = -1
	if m.backgroundMoreOffset >= 0 {
		m.backgroundMoreLine = start + m.backgroundMoreOffset
		m.backgroundEnd++
	}
}

func (m *model) backgroundOnlyHover() bool {
	if strings.HasPrefix(m.hoverTarget, "background:") {
		return true
	}
	if len(m.hoverValues) == 0 {
		return false
	}
	for key := range m.hoverValues {
		if !strings.HasPrefix(key, "background:") {
			return false
		}
	}
	return true
}

func (m *model) refreshBackgroundMotion() {
	if m.backgroundStart < 0 || len(m.cachedLines) == 0 {
		return
	}
	width := m.contentWidth(m.cachedNeedsScrollbar)
	patch := func(y int, id string, action ClickResult, payload, text string) {
		if y < 0 || y >= len(m.cachedLines) {
			return
		}
		if relative := y - m.backgroundStart; relative >= 0 && relative < len(m.backgroundLines) {
			m.backgroundLines[relative] = text
		}
		m.cachedLines[y] = text
		if m.placement == nil || m.placement.targetByID == nil {
			return
		}
		index, ok := m.placement.targetByID[id]
		if !ok || index < 0 || index >= len(m.placement.rows) {
			return
		}
		row := &m.placement.rows[index]
		if row.target && int(row.targetY) == y {
			row.id, row.action, row.payload, row.text = id, action, payload, text
			m.placement.painted = nil
		}
	}
	patch(m.backgroundHeader, "background:header", ClickNone, "", m.backgroundHeaderLine(width))
	limit := len(m.backgroundProcesses)
	if !m.backgroundShowAll {
		limit = min(limit, backgroundInitialRows)
	}
	if m.backgroundCollapsed && !m.backgroundDisclosureRunning {
		limit = 0
	}
	first := max(0, m.scrollview.ScrollOffset()-m.backgroundStart-1)
	last := min(limit, m.scrollview.ScrollOffset()+m.viewportHeight()-m.backgroundStart-1)
	now := m.backgroundNow()
	for index := first; index < last; index++ {
		process := m.backgroundProcesses[index]
		text := m.backgroundRow(process, width, now, index)
		if m.backgroundDisclosureRunning {
			text = styles.FadeLine(text, m.backgroundDisclosureAlpha)
		}
		payload, action := "", ClickNone
		if index < len(m.backgroundRowTools) {
			payload = m.backgroundRowTools[index]
		}
		if payload != "" {
			action = ClickBackgroundProcess
		}
		patch(m.backgroundStart+1+index, "background:process:"+process.ID, action, payload, text)
	}
	if limit > 0 && limit < len(m.backgroundProcesses) && m.backgroundMoreLine >= 0 {
		text := styles.MutedStyle.Render(fmt.Sprintf("  … +%d more", len(m.backgroundProcesses)-limit))
		patch(m.backgroundMoreLine, "background:more", ClickNone, "", text)
	}
	m.sectionCache["background"] = strings.Join(m.backgroundLines, "\n")
}

func (m *model) backgroundControlAt(x, y int) (string, bool) {
	if m.mode != ModeVertical || m.backgroundStart < 0 || x < m.layoutCfg.PaddingLeft || y < 0 || y >= m.viewportHeight() {
		return "", false
	}
	if m.placement != nil {
		row, ok := m.placementRowAt(x, y)
		if !ok {
			return "", false
		}
		if row.id == "background:header" {
			return "toggle", true
		}
		if row.id == "background:more" {
			return "more", true
		}
		return "", false
	}
	contentY := y + m.scrollview.ScrollOffset()
	if contentY == m.backgroundHeader {
		return "toggle", true
	}
	if contentY == m.backgroundMoreLine && m.backgroundMoreLine >= 0 {
		return "more", true
	}
	return "", false
}

func (m *model) toggleBackgroundControl(control string) tea.Cmd {
	switch control {
	case "toggle":
		m.backgroundManual = true
		m.backgroundDisclosureFrom = 0
		if m.backgroundCollapsed {
			m.backgroundCollapsed = false
			m.backgroundDisclosureAlpha = 0
			m.backgroundDisclosureTarget = 1
		} else {
			m.backgroundDisclosureTarget = 0
		}
		m.backgroundDisclosureRunning = true
	case "more":
		m.backgroundShowAll = true
		m.backgroundDisclosureFrom = backgroundInitialRows
		m.backgroundDisclosureAlpha = 0
		m.backgroundDisclosureTarget = 1
		m.backgroundDisclosureRunning = true
	}
	m.backgroundRowCache = nil
	m.invalidateBackgroundStructure()
	cmd := m.ReconcileLayout()
	return tea.Batch(cmd, m.syncBackgroundAnimation())
}

func (m *model) backgroundSummaryCollapsed() string {
	count := len(m.backgroundProcesses)
	if count == 0 {
		return ""
	}
	label := fmt.Sprintf("%d background", count)
	if m.backgroundRunning() {
		frame := m.backgroundFrame
		if frame == "" {
			frame = animation.Chat.FrameAt(m.ar.Now())
		}
		return styles.SpinnerDotsHighlightStyle.Render(frame) + " " + styles.MutedStyle.Render(label)
	}
	return styles.MutedStyle.Render(label)
}

func (m *model) invalidateBackgroundStructure() {
	if m.sectionCache != nil {
		delete(m.sectionCache, "background")
	}
	m.cacheInvalidations++
	m.cacheDirty = true
	m.layoutDirty = true
	m.visualGeneration++
	m.reconcileDirty = true
}
