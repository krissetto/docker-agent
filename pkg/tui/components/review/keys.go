package review

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// HandleKey consumes review keys. Unhandled keys (Tab, Shift+Tab, Ctrl/Alt
// chords such as Ctrl+C, Ctrl+K, Ctrl+N/P/T, function keys) belong to the host.
// Printable keys are always consumed so they never reach the composer.
func (m *Model) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	m.hint = ""
	handled, cmd := m.handleKey(msg)
	m.syncScrollviews()
	return handled, tea.Batch(cmd, m.selectionChanged())
}

func (m *Model) selectionChanged() tea.Cmd {
	sel := m.Selection()
	if sel == m.lastSelection {
		return nil
	}
	m.lastSelection = sel
	return m.emit(SelectionChangedMsg{Owner: m.owner, Selection: sel})
}

func (m *Model) handleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.filterActive {
		return m.inputKey(msg, &m.filterActive, true)
	}
	if m.searchOn {
		return m.inputKey(msg, &m.searchOn, false)
	}
	if m.editing {
		return m.editorKey(msg)
	}
	k := msg.String()
	if m.previewing() {
		if handled, cmd := m.previewKey(k); handled {
			return true, cmd
		}
		return printable(msg), nil
	}
	switch k {
	case "ctrl+w":
		m.hint = "Esc closes review"
		return true, nil
	case "ctrl+b":
		if m.inspector {
			return false, nil
		}
		m.navHidden = !m.navHidden
		m.invalidateRows()
		if m.navHidden {
			m.area = areaReader
		}
		m.reader.invalidate()
		m.ensureLineVisible()
		return true, nil
	case "q":
		return true, m.emit(CloseRequestMsg{Owner: m.owner})
	case "R":
		m.toggleDraft()
		return true, nil
	case "t":
		m.toggleSplit()
		return true, nil
	case "D":
		return true, m.emit(DockToggleRequestMsg{Owner: m.owner})
	case "r":
		clear(m.loadPending)
		m.hint = "Refreshing…"
		refresh := m.emit(RefreshRequestMsg{Owner: m.owner})
		if m.mode == ModeFiles {
			clear(m.dirs)
			m.invalidateRows()
			return true, tea.Batch(refresh, m.pendingFileLoads())
		}
		return true, refresh
	case "+", "-", "=":
		m.resizeNavigator(k)
		return true, nil
	case "{", "}":
		m.jumpHeader(map[string]int{"{": -1, "}": 1}[k])
		return true, m.preview()
	case "J", "K":
		return true, m.jumpFile(map[string]int{"K": -1, "J": 1}[k])
	}
	var handled bool
	var cmd tea.Cmd
	if m.draftOpen && m.area == areaNavigator {
		if handled, cmd = m.draftKey(k); handled {
			return true, cmd
		}
		return printable(msg), nil
	}
	if m.area == areaReader && m.readerVisible() {
		handled, cmd = m.readerKey(k)
	} else {
		handled, cmd = m.navigatorKey(k)
	}
	if handled {
		return true, cmd
	}
	return printable(msg), nil
}

func printable(msg tea.KeyPressMsg) bool {
	return msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModMeta|tea.ModSuper|tea.ModHyper) == 0
}

func (m *Model) readerVisible() bool {
	g := m.geometry()
	return g.showRead
}

func (m *Model) inputKey(msg tea.KeyPressMsg, active *bool, filter bool) (bool, tea.Cmd) {
	input := &m.search
	if filter {
		input = &m.filter
	}
	switch msg.String() {
	case "ctrl+c", "f1":
		return false, nil
	case "ctrl+n", "ctrl+p", "ctrl+t":
		return true, nil
	case "enter", "tab":
		*active = false
		input.Blur()
		if !filter {
			m.reader.jumpMatch(1, true)
			m.ensureLineVisible()
		}
		return true, m.preview()
	case "esc":
		*active = false
		input.Blur()
		input.Reset()
		m.afterInput(filter)
		if filter && m.filterBefore != "" {
			m.lastKey = m.filterBefore
			m.invalidateRows()
			m.currentRows()
		}
		return true, nil
	case "up", "down":
		if filter {
			m.moveCursor(map[string]int{"up": -1, "down": 1}[msg.String()])
			return true, nil
		}
		return true, nil
	}
	before := input.Value()
	updated, cmd := input.Update(msg)
	*input = updated
	if input.Value() != before {
		m.afterInput(filter)
	}
	return true, cmd
}

func (m *Model) afterInput(filter bool) {
	if filter {
		m.lastKey = ""
		m.invalidateRows()
		rows := m.currentRows()
		for i, r := range rows {
			if r.leaf() {
				m.cursor = i
				break
			}
		}
		m.rememberCursor()
		m.ensureCursorVisible()
		return
	}
	if m.hasDoc {
		m.reader.recomputeMatches(newMatcher(m.search.Value()))
		m.reader.jumpMatch(1, true)
		m.ensureLineVisible()
	}
}

func (m *Model) startFilter() tea.Cmd {
	m.currentRows()
	m.syncInputWidths()
	m.filterActive = true
	m.filterBefore = m.lastKey
	return m.filter.Focus()
}

func (m *Model) navigatorKey(k string) (bool, tea.Cmd) {
	h := max(1, m.bodyHeight())
	switch k {
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup":
		m.moveCursor(-h)
	case "pgdown":
		m.moveCursor(h)
	case "home", "g":
		m.cursor = 0
		m.snapToSelectable(1)
		m.cursorMoved()
	case "end", "G":
		m.cursor = len(m.currentRows()) - 1
		m.snapToSelectable(-1)
		m.cursorMoved()
	case "enter":
		return true, m.activate(true)
	case "space":
		return true, m.activate(false)
	case "right", "l":
		r, ok := m.selectedRow()
		if !ok {
			return true, nil
		}
		if r.structural() && r.expanded {
			m.moveCursor(1)
			return true, m.preview()
		}
		return true, m.activate(true)
	case "left", "h":
		m.collapseOrParent()
		return true, nil
	case "/":
		return true, m.startFilter()
	case "esc":
		if m.filter.Value() != "" {
			m.filter.Reset()
			m.afterInput(true)
			return true, nil
		}
		return true, m.emit(CloseRequestMsg{Owner: m.owner})
	case "c", "y":
		return true, m.copyPath()
	case "C":
		return true, m.copyWholePatch()
	default:
		return false, nil
	}
	return true, m.preview()
}

func (m *Model) readerKey(k string) (bool, tea.Cmd) {
	h := max(1, m.readerBodyHeight())
	w := m.readerWidth()
	r := &m.reader
	if !m.hasDoc || len(m.doc.Lines) == 0 {
		switch k {
		case "left", "h", "esc":
			if m.inspector && k == "esc" {
				return true, m.emit(CloseRequestMsg{Owner: m.owner})
			}
			m.backToNavigator()
			return true, nil
		}
		return false, nil
	}
	switch k {
	case "up", "k":
		r.line--
	case "down", "j":
		r.line++
	case "shift+up", "shift+down":
		if r.anchor < 0 {
			r.anchor = r.line
		}
		if k == "shift+up" {
			r.line--
		} else {
			r.line++
		}
	case "pgup":
		r.scroll(w, h, -h)
		return true, nil
	case "pgdown":
		r.scroll(w, h, h)
		return true, nil
	case "home", "g":
		r.line = 0
	case "end", "G":
		r.line = len(r.items) - 1
	case "]", "[":
		if !r.jumpHunk(map[string]int{"[": -1, "]": 1}[k]) {
			m.hint = "No more hunks"
		}
	case "n", "N":
		if m.search.Value() == "" {
			m.hint = "/ to search"
			return true, nil
		}
		if !r.jumpMatch(map[string]int{"N": -1, "n": 1}[k], false) {
			m.hint = "No matches"
		}
	case "/":
		m.syncInputWidths()
		m.searchOn = true
		return true, m.search.Focus()
	case "a":
		return true, m.startComment()
	case "v":
		if r.anchor >= 0 {
			r.anchor = -1
		} else {
			r.anchor = r.line
		}
	case "c":
		text, n := r.copyText()
		if n == 0 {
			return true, nil
		}
		return true, m.emit(CopyRequestMsg{Owner: m.owner, Kind: CopyText, Text: text, Summary: "Copied " + plural(n, "line") + " (text)"})
	case "C":
		if reason := r.patchDisabled(); reason != "" {
			m.hint = reason
			return true, nil
		}
		text, n := r.copyPatch(false)
		return true, m.emit(CopyRequestMsg{Owner: m.owner, Kind: CopyPatch, Text: text, Summary: "Copied " + plural(n, "hunk") + " (patch)"})
	case "y":
		return true, m.copyDocumentPath()
	case "left", "h":
		m.backToNavigator()
		return true, nil
	case "esc":
		switch {
		case r.anchor >= 0:
			r.anchor = -1
		case m.search.Value() != "":
			m.search.Reset()
			r.recomputeMatches(matcher{})
		case m.inspector:
			return true, m.emit(CloseRequestMsg{Owner: m.owner})
		default:
			m.backToNavigator()
		}
		return true, nil
	default:
		return false, nil
	}
	m.ensureLineVisible()
	return true, nil
}

func (m *Model) ensureLineVisible() {
	if m.hasDoc {
		m.reader.ensureVisible(m.readerWidth(), m.readerBodyHeight())
	}
}

func (m *Model) backToNavigator() {
	if m.inspector {
		return
	}
	m.area = areaNavigator
	m.drilled = false
	m.navHidden = false
	m.reader.invalidate()
}

func (m *Model) moveCursor(delta int) {
	rows := m.currentRows()
	if len(rows) == 0 {
		return
	}
	dir := 1
	if delta < 0 {
		dir = -1
	}
	m.cursor = max(0, min(len(rows)-1, m.cursor+delta))
	m.snapToSelectable(dir)
	m.cursorMoved()
}

func (m *Model) cursorMoved() {
	m.rememberCursor()
	m.ensureCursorVisible()
}

// preview loads the selected file into the reader while both panels are visible.
func (m *Model) preview() tea.Cmd {
	g := m.geometry()
	if !g.twoColumn {
		return nil
	}
	r, ok := m.selectedRow()
	if !ok || !r.leaf() {
		return nil
	}
	return m.open(r)
}

func (m *Model) activate(focusReader bool) tea.Cmd {
	r, ok := m.selectedRow()
	if !ok {
		return nil
	}
	switch {
	case r.kind == rowHidden:
		m.hint = "Outside · hidden"
		return nil
	case r.structural():
		return m.toggle(r)
	}
	cmd := m.open(r)
	if focusReader {
		m.area = areaReader
		if !m.geometry().twoColumn {
			m.drilled = true
		}
		m.reader.invalidate()
		m.ensureLineVisible()
	}
	return cmd
}

func (m *Model) open(r row) tea.Cmd {
	_, wt, ok := m.worktree(r)
	if !ok {
		return nil
	}
	key := DocumentKey{Mode: m.mode, WorktreeID: wt.ID, Path: r.path, Group: r.group}
	if m.hasRequest && m.requested == key {
		return nil
	}
	m.requested, m.hasRequest = key, true
	m.doc, m.hasDoc = Document{Key: key, State: StateLoading}, true
	m.reader.setDocument(&m.doc, false)
	return m.emit(OpenDocumentRequestMsg{Owner: m.owner, Key: key})
}

func (m *Model) toggle(r row) tea.Cmd {
	m.expanded[r.key] = !r.expanded
	var cmd tea.Cmd
	if !r.expanded {
		cmd = m.loadFor(r)
	}
	m.invalidateRows()
	return cmd
}

func (m *Model) loadFor(r row) tea.Cmd {
	_, wt, ok := m.worktree(r)
	if !ok {
		return nil
	}
	if m.mode == ModeChanges {
		if r.kind == rowWorktree && wt.State == StateUnscanned && !m.loadPending[r.key] {
			m.loadPending[r.key] = true
			return m.emit(LoadWorktreeRequestMsg{Owner: m.owner, WorktreeID: wt.ID})
		}
		return nil
	}
	p := ""
	if r.kind == rowDir {
		p = r.path
	} else if r.kind != rowWorktree {
		return nil
	}
	return m.loadDirectory(wt.ID, p)
}

func (m *Model) loadDirectory(worktreeID, p string) tea.Cmd {
	key := dirKey(worktreeID, p)
	if _, ok := m.dirs[dirID{worktreeID, p}]; ok || m.loadPending[key] {
		return nil
	}
	m.loadPending[key] = true
	return m.emit(LoadDirectoryRequestMsg{Owner: m.owner, WorktreeID: worktreeID, Path: p})
}

// pendingFileLoads requests folders that Files mode shows expanded but has not received.
func (m *Model) pendingFileLoads() tea.Cmd {
	if m.mode != ModeFiles {
		return nil
	}
	var cmds []tea.Cmd
	for _, r := range m.currentRows() {
		if r.expanded && (r.kind == rowWorktree || r.kind == rowDir) {
			cmds = append(cmds, m.loadFor(r))
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) collapseOrParent() {
	r, ok := m.selectedRow()
	if !ok {
		return
	}
	if r.structural() && r.expanded {
		m.expanded[r.key] = false
		m.invalidateRows()
		m.currentRows()
		return
	}
	rows := m.currentRows()
	for i := m.cursor - 1; i >= 0; i-- {
		if rows[i].depth < r.depth && rows[i].structural() {
			m.cursor = i
			m.cursorMoved()
			return
		}
	}
}

func (m *Model) jumpHeader(dir int) {
	rows := m.currentRows()
	for i := m.cursor + dir; i >= 0 && i < len(rows); i += dir {
		if rows[i].header() {
			m.cursor = i
			m.cursorMoved()
			return
		}
	}
}

func (m *Model) jumpFile(dir int) tea.Cmd {
	if m.inspector && m.hasRequest {
		return m.jumpInspectorFile(dir)
	}
	rows := m.currentRows()
	for i := m.cursor + dir; i >= 0 && i < len(rows); i += dir {
		if !rows[i].leaf() {
			continue
		}
		m.cursor = i
		m.cursorMoved()
		cmd := m.open(rows[i])
		m.ensureLineVisible()
		return cmd
	}
	m.hint = "No more files"
	return nil
}

// SetMode switches between Changes and Files; it may request folders to load.
func (m *Model) SetMode(mode Mode) tea.Cmd {
	if m.mode == mode {
		return nil
	}
	defer m.syncScrollviews()
	return m.toggleMode()
}

func (m *Model) toggleMode() tea.Cmd {
	if m.mode == ModeChanges {
		m.mode = ModeFiles
	} else {
		m.mode = ModeChanges
	}
	m.syncFilterPlaceholder()
	m.lastKey = ""
	m.cursor, m.navTop = 0, 0
	m.area = areaNavigator
	m.drilled = false
	m.invalidateRows()
	m.currentRows()
	return m.pendingFileLoads()
}

func (m *Model) toggleSplit() {
	if m.mode != ModeChanges {
		m.hint = "Split applies to diffs"
		return
	}
	m.splitPref = !m.splitPref
	if m.splitPref && !m.splitEffective() {
		m.hint = "Split shows when the reader is wider"
	}
	m.applySplit()
	m.ensureLineVisible()
}

func (m *Model) resizeNavigator(k string) {
	switch k {
	case "=":
		m.navWidth = 0
	case "+":
		m.navWidth = m.navigatorWidth() + navigatorStep
	case "-":
		m.navWidth = m.navigatorWidth() - navigatorStep
	}
	m.clampNavigator()
	m.reader.invalidate()
	m.invalidateRows()
	m.ensureLineVisible()
}

func (m *Model) copyPath() tea.Cmd {
	r, ok := m.selectedRow()
	if !ok {
		return nil
	}
	_, wt, _ := m.worktree(r)
	text := r.path
	if r.header() {
		text = wt.Path
	}
	if r.kind == rowHidden || text == "" {
		m.hint = "Nothing to copy"
		return nil
	}
	return m.emit(CopyRequestMsg{Owner: m.owner, Kind: CopyPath, Text: text, Summary: m.pathSummary()})
}

func (m *Model) copyDocumentPath() tea.Cmd {
	if !m.hasDoc || m.doc.Key.Path == "" {
		return nil
	}
	return m.emit(CopyRequestMsg{Owner: m.owner, Kind: CopyPath, Text: m.doc.Key.Path, Summary: m.pathSummary()})
}

func (m *Model) pathSummary() string {
	if m.snapshot.Scope.Remote {
		return "Copied remote path"
	}
	return "Copied path"
}

func (m *Model) copyWholePatch() tea.Cmd {
	r, ok := m.selectedRow()
	if !ok || !r.leaf() || !m.hasDoc || m.doc.Key.Path != r.path || m.doc.Key.Group != r.group {
		m.hint = "Open the file first"
		return nil
	}
	if reason := m.reader.patchDisabled(); reason != "" {
		m.hint = reason
		return nil
	}
	text, n := m.reader.copyPatch(true)
	return m.emit(CopyRequestMsg{Owner: m.owner, Kind: CopyPatch, Text: text, Summary: "Copied " + plural(n, "hunk") + " (patch)"})
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}

// HandleMouse owns every pointer event inside the review rectangle so nothing
// reaches the hidden chat underneath.
func (m *Model) HandleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	handled, cmd := m.handleMouse(msg)
	if handled {
		m.syncScrollviews()
	}
	return handled, cmd
}

func (m *Model) handleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	mouse := msg.Mouse()
	x, y := mouse.X-m.x, mouse.Y-m.y
	if x < 0 || y < 0 || x >= m.width || y >= m.height {
		return false, nil
	}
	m.hint = ""
	if m.scrollbarMouse(msg) {
		return true, nil
	}
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		delta := 0
		switch msg.Button {
		case tea.MouseWheelUp:
			delta = -3
		case tea.MouseWheelDown:
			delta = 3
		}
		m.wheel(x, delta)
		return true, nil
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return true, nil
		}
		cmd := m.click(x, y, msg.Mod&tea.ModShift != 0)
		var focus tea.Cmd
		if !m.focused {
			focus = m.emit(FocusRequestMsg{Owner: m.owner})
		}
		return true, tea.Batch(focus, cmd, m.selectionChanged())
	}
	return true, nil
}

func (m *Model) inNavigator(x int, g geometry) bool {
	return g.showNav && x >= g.navX && x < g.navX+g.navW
}

func (m *Model) inReader(x int, g geometry) bool {
	return g.showRead && x >= g.readerX && x < g.readerX+g.readerW
}

func (m *Model) wheel(x, delta int) {
	g := m.geometry()
	switch {
	case m.inNavigator(x, g):
		rows := m.currentRows()
		m.navTop = max(0, min(m.navTop+delta, max(0, len(rows)-g.navBodyH)))
	case m.inReader(x, g) && m.previewing():
		m.sendPreview.scroll(m.readerWidth(), g.readerBodyH, delta)
	case m.inReader(x, g) && m.hasDoc:
		m.reader.scroll(m.readerWidth(), g.readerBodyH, delta)
	}
}

// scrollbarMouse forwards scrollbar presses and drags to the shared scrollview
// and adopts the resulting offset.
func (m *Model) scrollbarMouse(msg tea.MouseMsg) bool {
	m.syncScrollviews()
	mouse := msg.Mouse()
	g := m.geometry()
	if g.showNav && (m.navSV.IsDragging() || m.navSV.IsMouseOnScrollbar(mouse.X, mouse.Y)) {
		if handled, _ := m.navSV.UpdateMouse(msg); handled {
			m.navTop = m.navSV.ScrollOffset()
			return true
		}
	}
	if g.showRead && m.hasDoc && (m.readSV.IsDragging() || m.readSV.IsMouseOnScrollbar(mouse.X, mouse.Y)) {
		if handled, _ := m.readSV.UpdateMouse(msg); handled {
			m.reader.scroll(m.readerWidth(), g.readerBodyH, m.readSV.ScrollOffset()-m.reader.top)
			return true
		}
	}
	return false
}

func (m *Model) click(x, y int, extend bool) tea.Cmd {
	g := m.geometry()
	switch {
	case m.inNavigator(x, g):
		m.area = areaNavigator
		lx := x - g.navX
		if y == 0 {
			if m.draftLabelAt(lx - navPadding) {
				m.toggleDraft()
			} else if m.draftOpen && lx-navPadding < ansi.StringWidth(m.modeLabel()) {
				m.toggleDraft()
			}
			return nil
		}
		if y < g.navBodyY {
			return m.startFilter()
		}
		if m.draftOpen {
			return m.draftClick(y-g.navBodyY, lx-navPadding, m.listWidth())
		}
		i := m.navTop + y - g.navBodyY
		rows := m.currentRows()
		if i >= len(rows) || !rows[i].selectable() {
			return nil
		}
		m.cursor = i
		m.cursorMoved()
		return m.activate(rows[i].leaf() && !g.twoColumn)
	case m.inReader(x, g):
		m.area = areaReader
		lx := x - g.readerX
		if y == 0 && m.previewing() {
			switch m.previewButtonAt(lx, g.readerW) {
			case sendLabel:
				_, cmd := m.previewKey("enter")
				return cmd
			case cancelLabel:
				_, cmd := m.previewKey("esc")
				return cmd
			}
			return nil
		}
		if m.previewing() {
			return nil
		}
		if y == 0 && m.commentAffordance() && lx >= g.readerW-ansi.StringWidth(commentLabel) {
			return m.startComment()
		}
		if y > 0 && y < g.readerBodyY {
			m.syncInputWidths()
			m.searchOn = true
			return m.search.Focus()
		}
		if !m.hasDoc || len(m.doc.Lines) == 0 || y < g.readerBodyY {
			return nil
		}
		m.reader.layout(m.readerWidth())
		row := m.reader.top + y - g.readerBodyY
		if row >= m.reader.total {
			return nil
		}
		if extend {
			if m.reader.anchor < 0 {
				m.reader.anchor = m.reader.line
			}
		} else {
			m.reader.anchor = -1
		}
		m.reader.line = m.reader.itemAtRow(row)
	}
	return nil
}

// HandlePaste routes paste to whichever review text input currently owns focus.
func (m *Model) HandlePaste(msg tea.PasteMsg) tea.Cmd {
	if !m.Capturing() {
		return nil
	}
	defer m.syncScrollviews()
	if m.editing {
		updated, cmd := m.editor.Update(msg)
		m.editor = updated
		return cmd
	}
	if m.filterActive {
		before := m.filter.Value()
		updated, cmd := m.filter.Update(msg)
		m.filter = updated
		if before != m.filter.Value() {
			m.afterInput(true)
		}
		return cmd
	}
	before := m.search.Value()
	updated, cmd := m.search.Update(msg)
	m.search = updated
	if before != m.search.Value() {
		m.afterInput(false)
	}
	return cmd
}

// jumpInspectorFile steps through the open worktree's changed files in group
// order, independent of what the sidebar has expanded.
func (m *Model) jumpInspectorFile(dir int) tea.Cmd {
	_, wt, ok := m.worktreeByID(m.requested.WorktreeID)
	if !ok {
		return nil
	}
	var keys []DocumentKey
	for _, g := range changeGroups {
		for _, f := range wt.Files {
			if f.Group == g {
				keys = append(keys, DocumentKey{Mode: ModeChanges, WorktreeID: wt.ID, Path: f.Path, Group: g})
			}
		}
	}
	for i, k := range keys {
		if k != m.requested {
			continue
		}
		if j := i + dir; j >= 0 && j < len(keys) {
			return m.OpenDocument(keys[j])
		}
	}
	m.hint = "No more files"
	return nil
}
