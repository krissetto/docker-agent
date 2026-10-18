package review

import (
	"maps"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
	"github.com/docker/docker-agent/pkg/tui/widgets/textinput"
)

type area int

const (
	areaNavigator area = iota
	areaReader
)

const (
	// twoColumnMinWidth is the narrowest rectangle that still shows both panels.
	twoColumnMinWidth   = 100
	navigatorDefault    = 42
	navigatorMin        = 24
	navigatorStep       = 4
	minRenderableHeight = 3
	// scrollReserve is the gap plus scrollbar column every panel body keeps.
	scrollReserve = 2
	// navPadding mirrors the sidebar's left content padding.
	navPadding = 1
)

// Model is one review surface. The host keeps one per workspace anchor.
type Model struct {
	owner         string
	width, height int
	x, y          int
	focused       bool

	snapshot Snapshot
	session  Session
	dirs     map[dirID]Directory

	mode      Mode
	area      area
	drilled   bool // narrow layout shows the reader screen
	navLeft   bool
	navHidden bool
	navWidth  int

	expanded map[string]bool
	rows     []row
	rowsOK   bool
	cursor   int
	navTop   int
	lastKey  string

	filter       textinput.Model
	filterActive bool
	filterBefore string

	requested   DocumentKey
	hasRequest  bool
	doc         Document
	hasDoc      bool
	reader      readerState
	search      textinput.Model
	searchOn    bool
	loadPending map[string]bool

	lastSelection Selection
	hint          string

	navSV, readSV *scrollview.Model
	revealed      bool
	inspector     bool // diff inspector: reader only; the sidebar is the navigator

	splitPref bool

	draft         DraftView
	draftOpen     bool
	draftCursor   int
	draftTop      int
	deleteArmed   string
	staleComments map[string]bool

	editing    bool
	editor     textarea.Model
	editAnchor Anchor
	editID     string

	sendPreview readerState
	previewDoc  Document
	pendingJump *Anchor
	docSeq      uint64
}

// ViewState captures presentation for exact return after workspace switches.
type ViewState struct {
	Mode       Mode
	Reader     bool
	Drilled    bool
	NavHidden  bool
	NavWidth   int
	Expanded   map[string]bool
	CursorKey  string
	NavTop     int
	Filter     string
	Document   DocumentKey
	HasDoc     bool
	Line       int
	Top        int
	Search     string
	SelectFrom int
	Split      bool
	Draft      bool
}

func New(owner string) *Model {
	m := &Model{owner: owner, dirs: map[dirID]Directory{}, expanded: map[string]bool{}, loadPending: map[string]bool{}}
	m.navSV = newPanelScrollview()
	m.readSV = newPanelScrollview()
	m.filter = newInput("filter changed files")
	m.syncFilterPlaceholder()
	m.search = newInput("search this file")
	m.reader.anchor = -1
	m.sendPreview.anchor = -1
	m.sendPreview.noCursor = true
	m.editor = newEditor()
	return m
}

// newPanelScrollview reuses the shared scrollbar and edge-fade policy with a
// reserved column so wrapping never shifts when the scrollbar appears.
func newPanelScrollview() *scrollview.Model {
	return scrollview.New(scrollview.WithKeyMap(nil), scrollview.WithReserveScrollbarSpace(true), scrollview.WithWheelStep(3))
}

// syncScrollviews mirrors the virtual offsets and totals into the scrollviews.
// Mutators call it so View only renders; it touches cached rows and layout only.
func (m *Model) syncScrollviews() {
	g := m.geometry()
	if g.showNav {
		m.navSV.SetSize(max(1, g.navW-navPadding), g.navBodyH)
		m.navSV.SetPosition(m.x+g.navX+navPadding, m.y+g.navBodyY)
		m.navSV.SetContent(nil, len(m.currentRows()))
		m.navSV.SetScrollOffset(m.navTop)
	}
	if g.showRead {
		r := m.activeReader()
		total := 0
		if r.doc != nil && len(r.items) > 0 {
			r.layout(m.readerWidth())
			total = r.total
		}
		m.readSV.SetSize(g.readerW, g.readerBodyH)
		m.readSV.SetPosition(m.x+g.readerX, m.y+g.readerBodyY)
		m.readSV.SetContent(nil, total)
		m.readSV.SetScrollOffset(r.top)
	}
}

func newInput(placeholder string) textinput.Model {
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = placeholder + "…"
	in.SetStyles(styles.DialogInputStyle)
	return in
}

func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) SetSize(width, height int) {
	m.width, m.height = max(0, width), max(0, height)
	// Narrowing to one column keeps the reader on screen if it had focus.
	m.drilled = m.area == areaReader && m.hasDoc && m.width < twoColumnMinWidth
	m.reader.invalidate()
	m.clampNavigator()
	m.invalidateRows()
	m.syncInputWidths()
	m.applySplit()
	m.ensureCursorVisible()
	m.ensureLineVisible()
	m.syncScrollviews()
}

func (m *Model) SetPosition(x, y int) {
	m.x, m.y = x, y
	m.syncScrollviews()
}

func (m *Model) SetNavigatorSide(left bool) { m.navLeft = left }

// SetSplitPreference applies the persisted Git diff layout without changing the
// review session's selection, draft, or navigation state.
func (m *Model) SetSplitPreference(split bool) {
	if m.splitPref == split {
		return
	}
	m.splitPref = split
	m.applySplit()
	m.syncScrollviews()
}

// SetInspector shows only the reader (and the draft list when toggled); the
// host's sidebar provides navigation. Esc in the reader asks to close.
func (m *Model) SetInspector(on bool) {
	m.inspector = on
	if on {
		m.area = areaReader
	}
	m.reader.invalidate()
	m.ensureLineVisible()
}

func (m *Model) navCollapsed() bool { return m.navHidden || (m.inspector && !m.draftOpen) }

// OpenKey reports the document the reader shows or is loading.
func (m *Model) OpenKey() (DocumentKey, bool) { return m.requested, m.hasRequest }

// OpenDocument shows a document by key, as if selected in the navigator.
func (m *Model) OpenDocument(key DocumentKey) tea.Cmd {
	if m.hasRequest && m.requested == key {
		return nil
	}
	return m.reloadDocument(key)
}

// ReloadDocument fetches the current contents of the already-open document.
func (m *Model) ReloadDocument(key DocumentKey) tea.Cmd {
	return m.reloadDocument(key)
}

func (m *Model) reloadDocument(key DocumentKey) tea.Cmd {
	m.area = areaReader
	keepDocument := m.hasDoc && m.doc.Key == key && m.doc.State == StateReady
	m.requested, m.hasRequest = key, true
	if !keepDocument {
		m.doc, m.hasDoc = Document{Key: key, State: StateLoading}, true
		m.reader.setDocument(&m.doc, false)
	}
	m.syncScrollviews()
	return m.emit(OpenDocumentRequestMsg{Owner: m.owner, Key: key})
}

func (m *Model) Focus()        { m.focused = true }
func (m *Model) Blur()         { m.focused = false }
func (m *Model) Focused() bool { return m.focused }
func (m *Model) Mode() Mode    { return m.mode }

func (m *Model) DocumentState() LoadState { return m.doc.State }

// Capturing reports an active text input; the host must not run navigation
// or tab shortcuts while it is true.
func (m *Model) Capturing() bool { return m.filterActive || m.searchOn || m.editing }

func (m *Model) SetSession(s Session) { m.session = s }

// SetSnapshot replaces the scanned data; in Files mode it may request folders shown expanded.
func (m *Model) SetSnapshot(s Snapshot) tea.Cmd {
	if s.Generation != m.snapshot.Generation {
		clear(m.loadPending)
	}
	m.snapshot = s
	m.revealCurrent()
	m.invalidateRows()
	defer m.syncScrollviews()
	return m.pendingFileLoads()
}

// revealCurrent opens only the path to the session's own checkout, once, so
// everything else starts collapsed.
func (m *Model) revealCurrent() {
	if m.revealed {
		return
	}
	for _, repo := range m.snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if !wt.Current {
				continue
			}
			m.revealed = true
			if _, set := m.expanded[repoKey(repo.ID)]; !set {
				m.expanded[repoKey(repo.ID)] = true
			}
			if _, set := m.expanded[worktreeKey(wt.ID)]; !set {
				m.expanded[worktreeKey(wt.ID)] = true
			}
			m.lastKey = worktreeKey(wt.ID)
			return
		}
	}
}

func (m *Model) SetDirectory(d Directory) tea.Cmd {
	m.dirs[dirID{d.WorktreeID, d.Path}] = d
	if d.State != StateLoading {
		delete(m.loadPending, dirKey(d.WorktreeID, d.Path))
	}
	if m.mode != ModeFiles {
		return nil
	}
	m.invalidateRows()
	defer m.syncScrollviews()
	return m.pendingFileLoads()
}

// SetDocument applies only the document most recently requested. The returned
// command colors it in the background (see ApplyHighlight).
func (m *Model) SetDocument(d Document) tea.Cmd {
	if !m.hasRequest || d.Key != m.requested {
		return nil
	}
	defer func() { m.docSeq++ }()
	same := m.hasDoc && m.doc.Key == d.Key && m.doc.State == StateReady
	m.doc, m.hasDoc = d, true
	m.reader.split = m.splitEffective()
	m.reader.setDocument(&m.doc, same)
	m.syncReaderComments()
	m.jumpToAnchor()
	m.reader.recomputeMatches(newMatcher(m.search.Value()))
	m.ensureLineVisible()
	m.syncScrollviews()
	job, seq, owner := m.reader.highlight, m.docSeq+1, m.owner
	m.reader.highlight = nil
	if job == nil {
		return nil
	}
	return func() tea.Msg { return HighlightedMsg{Owner: owner, seq: seq, tokens: job()} }
}

// ApplyHighlight installs tokens for the document they were computed for.
func (m *Model) ApplyHighlight(msg HighlightedMsg) {
	if msg.Owner != m.owner || msg.seq != m.docSeq || !m.hasDoc {
		return
	}
	m.reader.hl.tokens = msg.tokens
}

// invalidateRows rebuilds eagerly so View never mutates state.
func (m *Model) invalidateRows() {
	m.rowsOK = false
	m.currentRows()
}

func (m *Model) currentRows() []row {
	if !m.rowsOK {
		m.rows = m.buildRows()
		m.rowsOK = true
		m.restoreCursor()
	}
	return m.rows
}

func (m *Model) restoreCursor() {
	if m.lastKey != "" {
		for i, r := range m.rows {
			if r.key == m.lastKey {
				m.cursor = i
				m.ensureCursorVisible()
				return
			}
		}
	}
	m.cursor = max(0, min(m.cursor, len(m.rows)-1))
	m.snapToSelectable(1)
	m.rememberCursor()
	m.ensureCursorVisible()
}

func (m *Model) snapToSelectable(dir int) {
	for i := m.cursor; i >= 0 && i < len(m.rows); i += dir {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
	for i := m.cursor; i >= 0 && i < len(m.rows); i -= dir {
		if m.rows[i].selectable() {
			m.cursor = i
			return
		}
	}
}

func (m *Model) rememberCursor() {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		m.lastKey = m.rows[m.cursor].key
	}
}

func (m *Model) selectedRow() (row, bool) {
	rows := m.currentRows()
	if m.cursor < 0 || m.cursor >= len(rows) || !rows[m.cursor].selectable() {
		return row{}, false
	}
	return rows[m.cursor], true
}

// isExpanded defaults to collapsed; revealCurrent opens the session's own path.
func (m *Model) isExpanded(key string) bool { return m.expanded[key] }

func (m *Model) filterQuery() string { return m.filter.Value() }

// filterScope states what the filter searches, so Files mode never implies a global search.
func (m *Model) filterScope() string {
	if m.mode == ModeFiles {
		return "loaded folders only"
	}
	return "changed files"
}

func (m *Model) syncFilterPlaceholder() {
	m.filter.Placeholder = "filter " + m.filterScope() + "…"
}

func (m *Model) worktree(r row) (Repository, Worktree, bool) {
	if r.repo < 0 || r.repo >= len(m.snapshot.Repositories) {
		return Repository{}, Worktree{}, false
	}
	repo := m.snapshot.Repositories[r.repo]
	if r.wt < 0 || r.wt >= len(repo.Worktrees) {
		return repo, Worktree{}, false
	}
	return repo, repo.Worktrees[r.wt], true
}

func (m *Model) worktreeByID(id string) (Repository, Worktree, bool) {
	for _, repo := range m.snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == id {
				return repo, wt, true
			}
		}
	}
	return Repository{}, Worktree{}, false
}

// Selection reports the review's current target; the reader takes precedence when focused.
func (m *Model) Selection() Selection {
	if m.area == areaReader && m.hasDoc {
		repo, _, _ := m.worktreeByID(m.doc.Key.WorktreeID)
		return Selection{Mode: m.doc.Key.Mode, RepositoryID: repo.ID, WorktreeID: m.doc.Key.WorktreeID, Path: m.doc.Key.Path, Group: m.doc.Key.Group}
	}
	r, ok := m.selectedRow()
	if !ok {
		return Selection{Mode: m.mode}
	}
	repo, wt, _ := m.worktree(r)
	return Selection{Mode: m.mode, RepositoryID: repo.ID, WorktreeID: wt.ID, Path: r.path, Group: r.group}
}

// TargetMismatch reports when the reviewed checkout is not the session's own,
// so the host can label the composer, which always stays with the session.
func (m *Model) TargetMismatch() (string, bool) {
	sel := m.Selection()
	if sel.WorktreeID == "" || m.session.WorktreeID == "" || sel.WorktreeID == m.session.WorktreeID {
		return "", false
	}
	_, wt, ok := m.worktreeByID(sel.WorktreeID)
	if !ok {
		return "", false
	}
	return "reviewing " + worktreeLabel(wt) + " (not this session's folder)", true
}

func (m *Model) Save() ViewState {
	return ViewState{
		Mode: m.mode, Reader: m.area == areaReader, Drilled: m.drilled, NavHidden: m.navHidden, NavWidth: m.navWidth,
		Expanded: maps.Clone(m.expanded), CursorKey: m.lastKey, NavTop: m.navTop, Filter: m.filter.Value(),
		Document: m.requested, HasDoc: m.hasRequest, Line: m.reader.line, Top: m.reader.top, Search: m.search.Value(), SelectFrom: m.reader.anchor,
		Split: m.splitPref, Draft: m.draftOpen,
	}
}

// Restore reapplies saved presentation; the host re-supplies the document if it reopens one.
func (m *Model) Restore(s ViewState) tea.Cmd {
	m.mode, m.drilled, m.navHidden, m.navWidth = s.Mode, s.Drilled, s.NavHidden, s.NavWidth
	m.splitPref, m.draftOpen = s.Split, s.Draft
	m.area = areaNavigator
	if s.Reader {
		m.area = areaReader
	}
	m.expanded = maps.Clone(s.Expanded)
	if m.expanded == nil {
		m.expanded = map[string]bool{}
	}
	m.lastKey, m.navTop = s.CursorKey, s.NavTop
	m.revealed = true
	m.filter.SetValue(s.Filter)
	m.syncFilterPlaceholder()
	m.search.SetValue(s.Search)
	m.reader.line, m.reader.top, m.reader.anchor = s.Line, s.Top, s.SelectFrom
	m.reader.keepPosition = true
	m.invalidateRows()
	m.syncScrollviews()
	if s.HasDoc {
		m.requested, m.hasRequest = s.Document, true
		return m.emit(OpenDocumentRequestMsg{Owner: m.owner, Key: s.Document})
	}
	return nil
}

func (m *Model) emit(msg tea.Msg) tea.Cmd { return core.CmdHandler(msg) }

// layout geometry -------------------------------------------------------------

type geometry struct {
	twoColumn                bool
	navX, navW               int
	readerX, readerW         int
	sepX                     int
	showNav, showRead        bool
	navBodyY, navBodyH       int
	readerBodyY, readerBodyH int
}

func (m *Model) navHeaderRows() int {
	if m.filterActive || m.filter.Value() != "" {
		return 2
	}
	return 1
}

func (m *Model) readerHeaderRows() int {
	if !m.previewing() && (m.searchOn || m.search.Value() != "") {
		return 2
	}
	return 1
}

func (m *Model) geometry() geometry {
	g := geometry{sepX: -1, navBodyY: m.navHeaderRows(), readerBodyY: m.readerHeaderRows()}
	g.navBodyH = max(0, m.panelHeight()-g.navBodyY)
	g.readerBodyH = max(0, m.panelHeight()-g.readerBodyY)
	g.twoColumn = m.width >= twoColumnMinWidth && !m.navCollapsed()
	switch {
	case g.twoColumn:
		g.showNav, g.showRead = true, true
		g.navW = m.navigatorWidth()
		g.readerW = m.width - g.navW - 1
		if m.navLeft {
			g.navX, g.sepX, g.readerX = 0, g.navW, g.navW+1
		} else {
			g.readerX, g.sepX, g.navX = 0, g.readerW, g.readerW+1
		}
	case m.navCollapsed() || m.previewing() || (m.drilled && !(m.inspector && m.draftOpen)):
		g.showRead, g.readerW = true, m.width
	default:
		g.showNav, g.navW = true, m.width
	}
	return g
}

func (m *Model) navigatorWidth() int {
	w := m.navWidth
	if w == 0 {
		w = navigatorDefault
	}
	return min(max(w, navigatorMin), max(navigatorMin, m.width/2))
}

func (m *Model) clampNavigator() {
	if m.navWidth != 0 {
		m.navWidth = m.navigatorWidth()
	}
}

// readerPanelWidth is the reader panel width whether or not it is on screen.
func (m *Model) readerPanelWidth() int {
	g := m.geometry()
	if g.showRead {
		return g.readerW
	}
	if m.width >= twoColumnMinWidth {
		return m.width - m.navigatorWidth() - 1
	}
	return m.width
}

// readerWidth is the reader text width inside its scrollview.
func (m *Model) readerWidth() int { return max(1, m.readerPanelWidth()-scrollReserve) }

func (m *Model) navPanelWidth() int {
	if m.width >= twoColumnMinWidth && !m.navCollapsed() {
		return m.navigatorWidth()
	}
	return m.width
}

// listWidth is the navigator row width inside its padding and scrollview.
func (m *Model) listWidth() int { return max(1, m.navPanelWidth()-navPadding-scrollReserve) }

func (m *Model) syncInputWidths() {
	m.filter.SetWidth(max(1, m.navPanelWidth()-navPadding-ansi.StringWidth(m.filter.Prompt)-1))
	m.search.SetWidth(max(1, m.readerPanelWidth()-ansi.StringWidth(m.search.Prompt)-1))
}

func (m *Model) bodyHeight() int       { return max(0, m.panelHeight()-m.navHeaderRows()) }
func (m *Model) readerBodyHeight() int { return max(0, m.panelHeight()-m.readerHeaderRows()) }

// panelHeight leaves room for the comment editor, which spans both panels.
func (m *Model) panelHeight() int { return max(1, m.height-m.editorRows()) }

func (m *Model) ensureCursorVisible() {
	h := m.bodyHeight()
	if h <= 0 {
		return
	}
	if m.cursor < m.navTop {
		m.navTop = m.cursor
	}
	if m.cursor >= m.navTop+h {
		m.navTop = m.cursor - h + 1
	}
	m.navTop = max(0, min(m.navTop, max(0, len(m.rows)-h)))
}
