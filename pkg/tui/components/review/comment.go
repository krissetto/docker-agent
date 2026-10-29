package review

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func (m *Model) commentMatchesDocument(a Anchor, key DocumentKey) bool {
	return a.WorktreeID == key.WorktreeID && a.Path == key.Path && a.Mode == key.Mode && a.Group == key.Group
}

func commentStatus(a Anchor) string {
	start, end := a.NewStart, a.NewEnd
	if start == 0 {
		start, end = a.OldStart, a.OldEnd
	}
	if start == 0 {
		return "Comment"
	}
	if end <= start {
		return "Comment on L" + itoa(start)
	}
	return "Comment on L" + itoa(start) + "–" + itoa(end)
}

func documentKeyForAnchor(anchor Anchor) DocumentKey {
	return DocumentKey{Mode: anchor.Mode, WorktreeID: anchor.WorktreeID, Path: anchor.Path, Group: anchor.Group}
}

// prepareCommentDocumentChange prevents a pending inline edit from being
// detached from its file; save or explicitly cancel it before navigation.
func (m *Model) prepareCommentDocumentChange(key DocumentKey) bool {
	if m.commentInput == nil || documentKeyForAnchor(m.commentAnchor) == key {
		return true
	}
	m.hint = "Save or cancel the comment before changing files"
	return false
}

func (m *Model) startComment() tea.Cmd {
	if !m.hasDoc || m.doc.State != StateReady || m.previewing() {
		return nil
	}
	a, ok := m.reader.anchorFor()
	if !ok {
		m.hint = "Select diff lines to comment"
		return nil
	}
	return m.beginCommentInput(m.enrichAnchor(a), "", "")
}

func (m *Model) beginCommentInput(anchor Anchor, id, body string) tea.Cmd {
	if m.commentInput != nil {
		m.finishCommentInput(false)
	}
	item, ok := m.reader.commentItem(anchor)
	if !ok {
		m.hint = "The selected line is no longer available"
		return nil
	}
	input := editor.NewInput(editor.InputConfig{Placeholder: "Write a review comment…", NewlineKeys: append(core.EditorNewlineKeys(true), "shift+enter")})
	input.SetDynamicHeight(true)
	input.SetVirtualCursor(false)
	input.SetMaxContentHeight(5)
	if body != "" {
		input.SetValue(body)
	}
	m.commentInput, m.commentAnchor, m.commentID, m.commentItem = input, anchor, id, item
	m.dragging = false
	m.reader.line = item
	m.reader.inlineAfter = item
	m.reader.invalidate()
	m.syncReaderComments()
	m.hint = ""
	m.syncScrollviews()
	m.ensureLineVisible()
	m.syncScrollviews()
	return input.Init()
}

func (m *Model) finishCommentInput(save bool) tea.Cmd {
	if m.commentInput == nil {
		return nil
	}
	if m.commentSaving {
		m.hint = "Comment save is pending"
		return nil
	}
	body := strings.TrimSpace(m.commentInput.Value())
	id, anchor := m.commentID, m.commentAnchor
	if save && body != "" && (len(body) > 16<<10 || (id == "" && len(m.draft.Comments) >= 100) || (id == "" && anchorContextTooLarge(anchor))) {
		if len(body) > 16<<10 {
			m.hint = "Comment is too large · shorten it and try again"
		} else if id == "" && len(m.draft.Comments) >= 100 {
			m.hint = "Draft is full · send or delete a comment first"
		} else {
			m.hint = "Selection is too large · select fewer lines"
		}
		return nil
	}
	if !save || body == "" {
		m.reader.charSelection = nil
		m.reader.anchor = -1
		m.commentInput.Blur()
		m.commentInput = nil
		m.commentAnchor, m.commentID, m.commentItem = Anchor{}, "", -1
		m.reader.inlineAfter, m.reader.inlineLines = -1, nil
		m.reader.invalidate()
		m.ensureLineVisible()
		m.syncReaderComments()
		m.syncScrollviews()
		return nil
	}
	m.commentRequestSeq++
	requestID := fmt.Sprintf("%s:%d:%d", m.owner, m.mode, m.commentRequestSeq)
	m.pendingCommentRequest = requestID
	m.commentSaving = true
	m.hint = "Saving comment…"
	m.syncCommentInputSize()
	m.syncScrollviews()
	if id != "" {
		return m.emit(EditCommentMsg{Owner: m.owner, ID: id, Body: body, RequestID: requestID})
	}
	return m.emit(AddCommentMsg{Owner: m.owner, Anchor: anchor, Body: body, RequestID: requestID})
}

func (m *Model) syncCommentInputSize() {
	if m.commentInput == nil {
		m.reader.inlineAfter, m.reader.inlineLines = -1, nil
		return
	}
	width := max(1, m.readerWidth()-1)
	maxRows := max(1, min(5, m.readerBodyHeight()/3))
	m.commentInput.SetMaxContentHeight(maxRows)
	m.commentInput.SetDynamicHeight(true)
	m.commentInput.SetSize(width, maxRows+2)
	surface := strings.Split(m.commentInput.SurfaceView(), "\n")
	m.reader.inlineAfter = m.commentItem
	card := make([]string, 1+len(surface))
	card[0] = styles.InfoStyle.Render("▌") + styles.MutedStyle.Render(" "+commentStatus(m.commentAnchor)+" · Enter save · Esc cancel · Shift+Enter newline")
	for i, row := range surface {
		card[i+1] = styles.InfoStyle.Render("▌") + row
	}
	m.reader.inlineLines = make([]string, len(card))
	for i, row := range card {
		m.reader.inlineLines[i] = fit(row, m.readerWidth())
	}
	m.reader.invalidate()
}

func (m *Model) commentCardTop() int {
	if m.commentInput == nil || m.commentItem < 0 || m.commentItem >= len(m.reader.items) {
		return -1
	}
	m.reader.layout(m.readerWidth())
	return m.reader.starts[m.commentItem] + m.reader.textRows(m.commentItem, m.readerWidth()) + m.reader.commentRows(m.commentItem)
}

func (m *Model) commentCardClick(x, y int) bool {
	if m.commentInput == nil {
		return false
	}
	g := m.geometry()
	if !m.inReader(x, g) || y < g.readerBodyY || y >= g.readerBodyY+g.readerBodyH {
		return false
	}
	virtual := m.reader.top + y - g.readerBodyY
	top := m.commentCardTop()
	if virtual < top || virtual >= top+len(m.reader.inlineLines) {
		return false
	}
	m.commentInput.PlaceSurfaceCursor(max(0, x-g.readerX-1), max(0, virtual-top-1))
	return true
}

func anchorContextTooLarge(a Anchor) bool {
	if len(a.Context) > 400 {
		return true
	}
	n := 0
	for _, line := range a.Context {
		n += len(line)
	}
	return n > 256<<10
}

func (m *Model) commentInputHasText() bool {
	return m.commentInput != nil && strings.TrimSpace(m.commentInput.Value()) != ""
}

func (m *Model) commentKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.commentSaving {
		return true, nil
	}
	switch msg.String() {
	case "pgup", "pgdown":
		delta := m.readerBodyHeight()
		if msg.String() == "pgup" {
			delta = -delta
		}
		m.reader.scroll(m.readerWidth(), m.readerBodyHeight(), delta)
		m.syncScrollviews()
		return true, nil
	case "esc":
		return true, m.finishCommentInput(false)
	case "enter":
		return true, m.finishCommentInput(true)
	default:
		updated, cmd := m.commentInput.Update(msg)
		m.commentInput = updated
		m.syncCommentInputSize()
		m.ensureLineVisible()
		m.syncScrollviews()
		return true, cmd
	}
}

func (m *Model) commentPaste(msg tea.PasteMsg) tea.Cmd {
	if m.commentInput == nil || m.commentSaving {
		return nil
	}
	updated, cmd := m.commentInput.Update(msg)
	m.commentInput = updated
	m.hint = ""
	m.syncCommentInputSize()
	m.syncScrollviews()
	return cmd
}

func (m *Model) commentLineSelectable(virtualRow int) (int, bool) {
	m.reader.layout(m.readerWidth())
	if virtualRow < 0 || virtualRow >= m.reader.total {
		return 0, false
	}
	i := m.reader.itemAtRow(virtualRow)
	seg := virtualRow - m.reader.starts[i]
	if seg >= m.reader.textRows(i, m.readerWidth()) {
		return 0, false
	}
	line := m.doc.Lines[m.reader.primary(m.reader.itemAt(i))]
	return i, line.Kind == LineContext || line.Kind == LineAdd || line.Kind == LineRemove
}
