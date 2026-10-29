package review

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	sendLabel   = "Send"
	cancelLabel = "Cancel"
)

// previewing reports a captured send waiting for explicit confirmation.
func (m *Model) previewing() bool {
	return m.draft.Preview != "" && (m.draft.Status == DraftPreviewing || m.draft.Status == DraftSending || m.draft.Status == DraftFailed)
}

func (m *Model) activeReader() *readerState {
	if m.previewing() {
		return &m.sendPreview
	}
	return &m.reader
}

func (m *Model) splitEffective() bool {
	return m.splitPref && m.mode == ModeChanges && m.readerPanelWidth()-scrollReserve >= splitMinWidth
}

func (m *Model) applySplit() {
	if m.hasDoc {
		m.reader.setSplit(m.splitEffective())
		m.syncReaderComments()
	}
}

// SetDraft shows the host-owned review draft.
func (m *Model) SetDraft(d DraftView) {
	before := m.draft.Preview
	if m.commentInput != nil && !m.commentSaving && m.commentID != "" {
		found := false
		for _, comment := range d.Comments {
			if comment.ID == m.commentID {
				found = true
				break
			}
		}
		if !found {
			m.pendingCommentRequest = ""
			m.finishCommentInput(false)
		}
	}
	m.draft = d
	if d.Preview != before {
		m.previewDoc = Document{Key: DocumentKey{Mode: ModeFiles, Path: "review"}, State: StateReady}
		for i, line := range strings.Split(strings.TrimRight(d.Preview, "\n"), "\n") {
			m.previewDoc.Lines = append(m.previewDoc.Lines, Line{Kind: LineContext, New: i + 1, Text: line})
		}
		m.sendPreview.setDocument(&m.previewDoc, false)
		m.sendPreview.top = 0
	}
	m.draftCursor = max(0, min(m.draftCursor, m.draftRowCount()-1))
	m.syncReaderComments()
	m.syncScrollviews()
}

// syncReaderComments places draft comments under their anchored lines.
func (m *Model) syncReaderComments() {
	m.reader.comments = map[int][]commentNote{}
	m.staleComments = make(map[string]bool)
	if !m.hasDoc || m.doc.State != StateReady {
		return
	}
	for _, c := range m.draft.Comments {
		if m.commentInput != nil && c.ID == m.commentID {
			continue
		}
		a := c.Anchor
		if a.WorktreeID != m.doc.Key.WorktreeID || a.Path != m.doc.Key.Path || a.Mode != m.doc.Key.Mode || a.Group != m.doc.Key.Group {
			continue
		}
		if !m.reader.matchesAnchor(a) {
			m.staleComments[c.ID] = true
			continue
		}
		if i, ok := m.reader.commentItem(a); ok {
			if m.reader.comments == nil {
				m.reader.comments = map[int][]commentNote{}
			}
			expanded := m.commentExpanded[c.ID]
			m.reader.comments[i] = append(m.reader.comments[i], commentNote{ID: c.ID, Preview: commentPreview(c.Body), Body: strings.TrimSpace(c.Body), Expanded: expanded})
		}
	}
	m.reader.invalidate()
}

func commentPreview(body string) string {
	first, rest, more := strings.Cut(strings.TrimSpace(body), "\n")
	first = sanitize(first)
	if more && strings.TrimSpace(rest) != "" {
		first += " …"
	}
	return first
}

func lineRange(a Anchor) string {
	start, end, prefix := a.NewStart, a.NewEnd, "L"
	if start == 0 {
		start, end, prefix = a.OldStart, a.OldEnd, "old L"
	}
	if start == end {
		return prefix + strconv.Itoa(start)
	}
	return prefix + strconv.Itoa(start) + "–" + strconv.Itoa(end)
}

// enrichAnchor adds human-readable repository and checkout identity.
func (m *Model) enrichAnchor(a Anchor) Anchor {
	for _, repo := range m.snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == a.WorktreeID {
				a.RepositoryID, a.RepositoryName = repo.ID, repo.Name
				a.CheckoutPath, a.Branch, a.Detached = wt.Path, wt.Branch, wt.Detached
			}
		}
	}
	for _, f := range m.currentFiles(a.WorktreeID) {
		if f.Path == a.Path && f.Group == a.Group {
			a.OldPath = f.OldPath
		}
	}
	return a
}

func (m *Model) currentFiles(worktreeID string) []FileChange {
	if _, wt, ok := m.worktreeByID(worktreeID); ok {
		return wt.Files
	}
	return nil
}

func (m *Model) startEdit(id string) tea.Cmd {
	for _, c := range m.draft.Comments {
		if c.ID != id {
			continue
		}
		key := documentKeyForAnchor(c.Anchor)
		if !m.prepareCommentDocumentChange(key) {
			return nil
		}
		if m.hasDoc && m.doc.Key == key && m.doc.State == StateReady {
			return m.beginCommentInput(c.Anchor, c.ID, c.Body)
		}
		copy := c
		m.pendingComment = &copy
		m.mode = key.Mode
		m.draftOpen = false
		m.revealFile(key)
		m.area = areaReader
		if !m.geometry().twoColumn {
			m.drilled = true
		}
		m.requested, m.hasRequest = key, true
		m.doc, m.hasDoc = Document{Key: key, State: StateLoading}, true
		m.reader.setDocument(&m.doc, false)
		m.syncScrollviews()
		return m.emit(OpenDocumentRequestMsg{Owner: m.owner, Key: key})
	}
	return nil
}

// Draft list ---------------------------------------------------------------

// draftRowCount includes the trailing "Send review…" action when non-empty.
func boolIntValue(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (m *Model) draftRowCount() int {
	if len(m.draft.Comments) == 0 && strings.TrimSpace(m.draft.General) == "" {
		return 0
	}
	return len(m.draft.Comments) + 1 + boolIntValue(strings.TrimSpace(m.draft.General) != "")
}

func (m *Model) toggleDraft() {
	m.draftOpen = !m.draftOpen
	m.area = areaNavigator
	if m.inspector && !m.draftOpen {
		m.area = areaReader
	}
	m.drilled = false
	m.deleteArmed = ""
	m.invalidateRows()
	m.reader.invalidate()
	m.ensureLineVisible()
}

func (m *Model) draftKey(k string) (bool, tea.Cmd) {
	n := m.draftRowCount()
	switch k {
	case "up", "k":
		m.draftCursor = max(0, m.draftCursor-1)
	case "down", "j":
		m.draftCursor = min(max(0, n-1), m.draftCursor+1)
	case "home", "g":
		m.draftCursor = 0
	case "end", "G":
		m.draftCursor = max(0, n-1)
	case "esc", "R":
		m.toggleDraft()
	case "enter", "right", "l":
		return true, m.activateDraftRow()
	case "e":
		if m.draftCursor < len(m.draft.Comments) {
			return true, m.startEdit(m.draft.Comments[m.draftCursor].ID)
		}
		if m.draft.General != "" && m.draftCursor == len(m.draft.Comments) {
			return true, m.emit(GeneralCommentRequestMsg{Owner: m.owner})
		}
	case "d", "delete":
		return true, m.deleteDraftRow()
	case "s":
		return true, m.requestPreview()
	default:
		return false, nil
	}
	m.ensureDraftVisible()
	return true, nil
}

func (m *Model) ensureDraftVisible() {
	h := max(1, m.bodyHeight())
	if m.draftCursor < m.draftTop {
		m.draftTop = m.draftCursor
	}
	if m.draftCursor >= m.draftTop+h {
		m.draftTop = m.draftCursor - h + 1
	}
}

func (m *Model) deleteDraftRow() tea.Cmd {
	if m.draftCursor >= len(m.draft.Comments) {
		return nil
	}
	id := m.draft.Comments[m.draftCursor].ID
	if m.deleteArmed != id {
		m.deleteArmed = id
		m.hint = "Press d again to delete"
		return nil
	}
	m.deleteArmed = ""
	return m.emit(DeleteCommentMsg{Owner: m.owner, ID: id})
}

func (m *Model) activateDraftRow() tea.Cmd {
	if m.draft.General != "" && m.draftCursor == len(m.draft.Comments) {
		return m.emit(GeneralCommentRequestMsg{Owner: m.owner})
	}
	if m.draftCursor >= len(m.draft.Comments)+boolIntValue(m.draft.General != "") {
		return m.requestPreview()
	}
	a := m.draft.Comments[m.draftCursor].Anchor
	if a.Mode != m.mode {
		m.hint = "Open in " + map[Mode]string{ModeChanges: "/changes", ModeFiles: "/files"}[a.Mode]
		return nil
	}
	m.draftOpen = false
	key := DocumentKey{Mode: a.Mode, WorktreeID: a.WorktreeID, Path: a.Path, Group: a.Group}
	m.revealFile(key)
	m.area = areaReader
	if !m.geometry().twoColumn {
		m.drilled = true
	}
	m.pendingJump = &a
	if m.hasRequest && m.requested == key {
		m.jumpToAnchor()
		return nil
	}
	m.requested, m.hasRequest = key, true
	m.doc, m.hasDoc = Document{Key: key, State: StateLoading}, true
	m.reader.setDocument(&m.doc, false)
	return m.emit(OpenDocumentRequestMsg{Owner: m.owner, Key: key})
}

// revealFile expands the path to a file and selects it in the navigator.
func (m *Model) revealFile(key DocumentKey) {
	for _, repo := range m.snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.ID == key.WorktreeID {
				m.expanded[repoKey(repo.ID)] = true
				m.expanded[worktreeKey(wt.ID)] = true
				m.expanded[groupKey(wt.ID, key.Group)] = true
			}
		}
	}
	m.lastKey = fileKey(key.WorktreeID, key.Group, key.Path)
	m.invalidateRows()
}

func (m *Model) jumpToAnchor() {
	a := m.pendingJump
	m.pendingJump = nil
	if a == nil || !m.hasDoc {
		return
	}
	if i, ok := m.reader.commentItem(*a); ok {
		m.reader.line, m.reader.anchor = i, -1
		m.ensureLineVisible()
	}
}

func (m *Model) requestPreview() tea.Cmd {
	if len(m.draft.Comments) == 0 && strings.TrimSpace(m.draft.General) == "" {
		m.hint = "Draft is empty"
		return nil
	}
	if m.draft.Status == DraftSending {
		return nil
	}
	return m.emit(PreviewDraftMsg{Owner: m.owner, Revision: m.draft.Revision})
}

func (m *Model) previewKey(k string) (bool, tea.Cmd) {
	r := &m.sendPreview
	h := max(1, m.readerBodyHeight())
	w := m.readerWidth()
	switch k {
	case "enter":
		if m.draft.Status == DraftSending {
			return true, nil
		}
		return true, m.emit(SendDraftMsg{Owner: m.owner, Revision: m.draft.Revision, Token: m.draft.Token})
	case "esc", "q":
		if m.draft.Status == DraftSending {
			return true, nil
		}
		return true, m.emit(CancelPreviewMsg{Owner: m.owner})
	case "up", "k":
		r.scroll(w, h, -1)
	case "down", "j":
		r.scroll(w, h, 1)
	case "pgup":
		r.scroll(w, h, -h)
	case "pgdown", "space":
		r.scroll(w, h, h)
	case "home", "g":
		r.scroll(w, h, -r.total)
	case "end", "G":
		r.scroll(w, h, r.total)
	case "tab", "shift+tab":
		return false, nil
	}
	return true, nil
}

func (m *Model) previewTitle(width int) string {
	title := titleStyle(true).Render("Send review to " + sanitize(fallback(m.draft.Target, "…")))
	var status string
	switch m.draft.Status {
	case DraftSending:
		status = styles.MutedStyle.Render("sending…")
	case DraftFailed:
		status = styles.ErrorStyle.Render(sanitize(fallback(m.draft.Err, "send failed"))) + " " + m.previewButtons()
	default:
		status = m.previewButtons()
	}
	return heading(title, status, width)
}

func (m *Model) previewButtons() string {
	return styles.HighlightWhiteStyle.Render("["+sendLabel+"]") + " " + styles.MutedStyle.Render("["+cancelLabel+"]")
}

// previewButtonAt maps a reader heading column to Send/Cancel.
func (m *Model) previewButtonAt(x, width int) string {
	cancel := ansi.StringWidth("[" + cancelLabel + "]")
	send := ansi.StringWidth("[" + sendLabel + "]")
	switch {
	case x >= width-cancel && x < width:
		return cancelLabel
	case x >= width-cancel-1-send && x < width-cancel-1:
		return sendLabel
	}
	return ""
}

func (m *Model) draftRows(width int) []string {
	if len(m.draft.Comments) == 0 && strings.TrimSpace(m.draft.General) == "" {
		return []string{styles.MutedStyle.Italic(true).Render("No comments yet · select lines and press c")}
	}
	focused := m.navFocused()
	var out []string
	for i := m.draftTop; i < m.draftRowCount() && len(out) < m.bodyHeight(); i++ {
		selected := i == m.draftCursor
		mark := " "
		if selected {
			mark = styles.MutedStyle.Render("▌")
			if focused {
				mark = styles.HighlightWhiteStyle.Render("▌")
			}
		}
		if m.draft.General != "" && i == len(m.draft.Comments) {
			out = append(out, mark+" "+styles.SecondaryStyle.Render("General comment")+"  "+ansi.Truncate(commentPreview(m.draft.General), max(1, width-20), "…"))
			continue
		}
		if i == len(m.draft.Comments)+boolIntValue(m.draft.General != "") {
			label := "Send review…"
			if m.draft.Status == DraftFailed {
				label = "Retry send…"
			}
			out = append(out, mark+" "+styles.TabTitleStyle.Bold(true).Render(label))
			continue
		}
		c := m.draft.Comments[i]
		where := truncateMiddle(sanitize(c.Anchor.Path), max(8, width/2)) + ":" + lineRange(c.Anchor)
		actions := ""
		if selected {
			actions = styles.MutedStyle.Render("✎ ✕")
		}
		if m.staleComments[c.ID] {
			where = "⚠ stale · " + where
		}
		left := mark + " " + styles.SecondaryStyle.Render(where) + "  " + commentPreview(c.Body)
		left = ansi.Truncate(left, max(1, width-ansi.StringWidth(actions)-1), "…")
		out = append(out, left+strings.Repeat(" ", max(1, width-ansi.StringWidth(left)-ansi.StringWidth(actions)))+actions)
	}
	return out
}

func (m *Model) draftClick(row, x, width int) tea.Cmd {
	i := m.draftTop + row
	if i >= m.draftRowCount() {
		return nil
	}
	if i == m.draftCursor && i < len(m.draft.Comments) {
		switch {
		case x >= width-1:
			m.deleteArmed = m.draft.Comments[i].ID
			return m.deleteDraftRow()
		case x >= width-3 && x < width-2:
			return m.startEdit(m.draft.Comments[i].ID)
		}
	}
	m.draftCursor = i
	if m.draft.General != "" && i == len(m.draft.Comments) {
		return m.emit(GeneralCommentRequestMsg{Owner: m.owner})
	}
	if i == len(m.draft.Comments)+boolIntValue(m.draft.General != "") {
		return m.requestPreview()
	}
	return nil
}
