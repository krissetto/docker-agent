package review

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
)

const (
	editorTextRows = 3
	commentLabel   = "＋ comment"
	sendLabel      = "Send"
	cancelLabel    = "Cancel"
)

func newEditor() textarea.Model {
	ed := textarea.New()
	ed.Placeholder = "Comment for the agent…"
	ed.SetStyles(styles.InputStyle)
	ed.SetHeight(editorTextRows)
	return ed
}

func (m *Model) editorRows() int {
	if !m.editing {
		return 0
	}
	return editorTextRows + 1
}

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
	m.reader.comments = nil
	m.staleComments = make(map[string]bool)
	if !m.hasDoc || m.doc.State != StateReady {
		return
	}
	for _, c := range m.draft.Comments {
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
				m.reader.comments = map[int][]string{}
			}
			m.reader.comments[i] = append(m.reader.comments[i], commentPreview(c.Body))
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

// startComment opens the editor for the selected lines (or the cursor line).
func (m *Model) startComment() tea.Cmd {
	if !m.hasDoc || m.doc.State != StateReady || m.previewing() {
		return nil
	}
	a, ok := m.reader.anchorFor()
	if !ok {
		m.hint = "Select diff lines to comment"
		return nil
	}
	m.editAnchor, m.editID = m.enrichAnchor(a), ""
	return m.openEditor("")
}

func (m *Model) startEdit(id string) tea.Cmd {
	for _, c := range m.draft.Comments {
		if c.ID == id {
			m.editAnchor, m.editID = c.Anchor, id
			return m.openEditor(c.Body)
		}
	}
	return nil
}

func (m *Model) openEditor(body string) tea.Cmd {
	m.editing = true
	m.editor.SetWidth(max(1, m.width-2))
	m.editor.SetValue(body)
	m.ensureLineVisible()
	return m.editor.Focus()
}

func (m *Model) closeEditor() {
	m.editing = false
	m.editor.Blur()
	m.editor.Reset()
	m.ensureLineVisible()
}

func (m *Model) editorKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "f1":
		return false, nil
	case "ctrl+n", "ctrl+p", "ctrl+t":
		return true, nil
	case "esc":
		m.closeEditor()
		return true, nil
	case "ctrl+j", "shift+enter":
		m.editor.InsertString("\n")
		return true, nil
	case "enter":
		body := strings.TrimSpace(m.editor.Value())
		if body == "" {
			m.hint = "Comment is empty"
			return true, nil
		}
		var cmd tea.Cmd
		if m.editID != "" {
			cmd = m.emit(EditCommentMsg{Owner: m.owner, ID: m.editID, Body: body})
		} else {
			cmd = m.emit(AddCommentMsg{Owner: m.owner, Anchor: m.editAnchor, Body: body})
			m.reader.anchor = -1
		}
		m.closeEditor()
		return true, cmd
	}
	updated, cmd := m.editor.Update(msg)
	m.editor = updated
	return true, cmd
}

func (m *Model) renderEditor() []string {
	what := "Comment"
	if m.editID != "" {
		what = "Edit comment"
	}
	label := styles.TabTitleStyle.Bold(true).Render(what) + styles.MutedStyle.Render(" · "+sanitize(m.editAnchor.Path)+" "+lineRange(m.editAnchor)+" · Enter save · Ctrl+J newline · Esc cancel")
	out := []string{fit(" "+label, m.width)}
	for line := range strings.SplitSeq(m.editor.View(), "\n") {
		out = append(out, fit(" "+line, m.width))
	}
	return padRows(out, m.width, m.editorRows())
}

// Draft list ---------------------------------------------------------------

// draftRowCount includes the trailing "Send review…" action when non-empty.
func (m *Model) draftRowCount() int {
	if len(m.draft.Comments) == 0 {
		return 0
	}
	return len(m.draft.Comments) + 1
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
	if m.draftCursor >= len(m.draft.Comments) {
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
	if len(m.draft.Comments) == 0 {
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
	if len(m.draft.Comments) == 0 {
		return []string{styles.MutedStyle.Italic(true).Render("No comments yet · select lines and press a")}
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
		if i == len(m.draft.Comments) {
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
	if i == len(m.draft.Comments) {
		return m.requestPreview()
	}
	return nil
}
