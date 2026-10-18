package review

import (
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threeHunks has a long final hunk with wrapping lines, the shape that used
// to leave the last hunk unreachable.
func threeHunks(key DocumentKey) Document {
	doc := Document{Key: key, State: StateReady, PatchHeader: "diff --git a/x b/x\n--- a/x\n+++ b/x"}
	n := 1
	for h := range 3 {
		doc.Lines = append(doc.Lines, Line{Kind: LineHunk, Text: "@@ hunk " + strconv.Itoa(h+1) + " @@"})
		count := 3
		if h == 2 {
			count = 40
		}
		for i := range count {
			text := "line " + strconv.Itoa(n)
			if h == 2 && i%5 == 0 {
				text += " " + strings.Repeat("wrapping text ", 12)
			}
			kind := LineContext
			switch i % 3 {
			case 1:
				kind = LineRemove
			case 2:
				kind = LineAdd
			}
			l := Line{Kind: kind, Text: text}
			if kind != LineAdd {
				l.Old = n
			}
			if kind != LineRemove {
				l.New = n
			}
			doc.Lines = append(doc.Lines, l)
			n++
		}
	}
	doc.Lines = append(doc.Lines, Line{Kind: LineContext, Old: n, New: n, Text: "final line"})
	return doc
}

func openThreeHunks(t *testing.T, w, h int, split bool) *Model {
	t.Helper()
	m := newModel(t, w, h)
	m.splitPref = split
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(threeHunks(req.Key))
	m.area = areaReader
	return m
}

func lastRowVisible(m *Model) bool {
	return strings.Contains(ansi.Strip(m.View()), "final line")
}

func TestFinalHunkIsReachableEveryWay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		w, h  int
		split bool
	}{
		{"unified narrow short", 60, 8, false},
		{"unified wide", 140, 12, false},
		{"split wide", 160, 12, true},
		{"split preference narrow", 70, 8, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, way := range [][]string{{"G"}, {"pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown", "pgdown"}, {"]", "]", "]"}} {
				m := openThreeHunks(t, tc.w, tc.h, tc.split)
				keys(t, m, way...)
				if way[0] == "]" {
					assert.Equal(t, 3, func() int { c, _ := m.reader.hunkPosition(); return c }(), "] reaches hunk 3")
					keys(t, m, "G")
				}
				cur, total := m.reader.hunkPosition()
				assert.Equal(t, 3, total)
				assert.Equal(t, 3, cur, "%v ends in the last hunk", way)
				assert.True(t, lastRowVisible(m), "%v shows the last line", way)
				assert.Equal(t, m.readSV.MaxScrollOffset(), m.reader.top)
			}
			m := openThreeHunks(t, tc.w, tc.h, tc.split)
			g := m.geometry()
			for range 200 {
				m.HandleMouse(tea.MouseWheelMsg{X: g.readerX + 3, Y: g.readerBodyY + 1, Button: tea.MouseWheelDown})
			}
			cur, _ := m.reader.hunkPosition()
			assert.Equal(t, 3, cur, "wheel to the end counts the last hunk")
			assert.True(t, lastRowVisible(m))
		})
	}
}

func TestFinalHunkStaysReachableAfterResizeAndComments(t *testing.T) {
	m := openThreeHunks(t, 140, 12, false)
	keys(t, m, "G")
	m.SetSize(60, 9)
	keys(t, m, "G")
	assert.True(t, lastRowVisible(m), "resize recomputes wrapped rows")
	a, _ := m.reader.anchorFor()
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: a, Body: "check this"}}, Revision: 1})
	keys(t, m, "g", "G")
	assert.True(t, lastRowVisible(m), "comment rows extend the scroll range")
	assert.Contains(t, ansi.Strip(m.View()), "┊ check this")
}

func TestSplitAlignsOldAndNewRows(t *testing.T) {
	m := newModel(t, 160, 14)
	m.splitPref = true
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	require.True(t, m.reader.split)
	view := ansi.Strip(m.View())
	t.Log("\n" + view)
	var row string
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, `label := ""`) {
			row = line
		}
	}
	require.NotEmpty(t, row)
	assert.Contains(t, row, `label, hint := "", ""`, "the removed and added lines share one row")
	assert.Less(t, strings.Index(row, `- label := ""`), strings.Index(row, `+ label, hint`), "old on the left, new on the right")

	keys(t, m, "t")
	assert.False(t, m.reader.split)
	m.SetSize(80, 14)
	keys(t, m, "t")
	assert.True(t, m.splitPref, "the preference is kept")
	assert.False(t, m.reader.split, "narrow readers fall back to unified")
	m.SetSize(160, 14)
	assert.True(t, m.reader.split, "widening restores split")
	assertFrame(t, m.View(), 160, 14)
}

func TestSyntaxHighlightingTokenizesOnceAndHandlesMultiline(t *testing.T) {
	m := newModel(t, 140, 12)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	doc := Document{Key: req.Key, State: StateReady, Lines: []Line{
		{Kind: LineHunk, Text: "@@ -1,4 +1,4 @@"},
		{Kind: LineContext, Old: 1, New: 1, Text: "/* opening comment"},
		{Kind: LineContext, Old: 2, New: 2, Text: "still a comment */"},
		{Kind: LineRemove, Old: 3, Text: "var old = 1"},
		{Kind: LineAdd, New: 3, Text: "var updated = \"two\""},
	}}
	before := tokenizeCalls.Load()
	cmd := m.SetDocument(doc)
	assert.Equal(t, before, tokenizeCalls.Load(), "SetDocument never tokenizes on the event loop")
	assert.NotContains(t, ansi.Strip(m.View()), "Loading", "plain text shows immediately")
	stale := cmd().(HighlightedMsg)
	assert.Equal(t, int64(2), tokenizeCalls.Load()-before, "old and new streams tokenize once each")
	m.ApplyHighlight(stale)
	require.NotNil(t, m.reader.hl.tokens[2])
	assert.True(t, m.reader.hl.tokens[2][0].typ.InCategory(chroma.Comment), "a comment continued from the previous line stays a comment")
	assert.True(t, m.reader.hl.tokens[3][0].typ.InCategory(chroma.Keyword), "removed lines are highlighted from the old stream")

	calls := tokenizeCalls.Load()
	first := m.View()
	m.SetSize(100, 12)
	m.View()
	m.SetSize(140, 12)
	assert.Equal(t, first, m.View(), "resizing reuses tokens")
	assert.Equal(t, calls, tokenizeCalls.Load(), "View and resize never tokenize")

	m.reader.line = 4
	copied, _ := find[CopyRequestMsg](keys(t, m, "c"))
	assert.Equal(t, "+var updated = \"two\"\n", copied.Text, "copies stay raw text")

	huge := Document{Key: req.Key, State: StateReady}
	for i := range highlightMaxLines + 1 {
		huge.Lines = append(huge.Lines, Line{Kind: LineContext, Old: i + 1, New: i + 1, Text: "x := 1"})
	}
	assert.Nil(t, m.SetDocument(huge), "oversized documents render plain")
	m.ApplyHighlight(stale)
	assert.Nil(t, m.reader.hl.tokens, "results for an older document are ignored")
	assert.Contains(t, ansi.Strip(m.View()), "x := 1")
}

func TestCommentDraftFlowByKeyboard(t *testing.T) {
	m := newModel(t, 140, 16)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	m.reader.line = 2
	keys(t, m, "v", "j", "a")
	require.True(t, m.editing)
	require.True(t, m.Capturing())
	keys(t, m, "W", "h", "y")
	m.HandleKey(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	keys(t, m, "?")
	add, ok := find[AddCommentMsg](keys(t, m, "enter"))
	require.True(t, ok)
	assert.Equal(t, "Why\n?", add.Body)
	assert.Equal(t, 2, add.Anchor.OldStart)
	assert.Equal(t, 2, add.Anchor.NewEnd)
	assert.Equal(t, "docker-agent", add.Anchor.RepositoryName)
	assert.Equal(t, "main", add.Anchor.Branch)
	assert.Equal(t, []string{`-label := ""`, `+label, hint := "", ""`}, add.Anchor.Context)
	assert.Len(t, add.Anchor.Version, 16)
	assert.False(t, m.editing)

	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: add.Anchor, Body: add.Body}}, Revision: 1})
	assert.Contains(t, ansi.Strip(m.View()), "Draft 1")
	assert.Contains(t, ansi.Strip(m.View()), "┊ Why …", "the comment shows under its lines")

	keys(t, m, "R")
	require.True(t, m.draftOpen)
	assert.Contains(t, ansi.Strip(m.View()), "pkg/tui/panel.go:L2")
	edit, ok := find[EditCommentMsg](keys(t, m, "e", "!", "enter"))
	require.True(t, ok)
	assert.Equal(t, "c1", edit.ID)
	assert.Equal(t, "Why\n?!", edit.Body)

	_, deleted := find[DeleteCommentMsg](keys(t, m, "d"))
	assert.False(t, deleted, "delete needs confirmation")
	del, ok := find[DeleteCommentMsg](keys(t, m, "d"))
	require.True(t, ok)
	assert.Equal(t, "c1", del.ID)

	preview, ok := find[PreviewDraftMsg](keys(t, m, "s"))
	require.True(t, ok, "s previews; nothing is sent yet")
	assert.Equal(t, uint64(1), preview.Revision)

	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: add.Anchor, Body: add.Body}}, Revision: 1, Status: DraftPreviewing, Target: "◆ main session", Preview: "Review comments\n\npkg/tui/panel.go L2\nWhy?", Token: "tok-1"})
	view := ansi.Strip(m.View())
	assert.Contains(t, view, "Send review to ◆ main session")
	assert.Contains(t, view, "pkg/tui/panel.go L2")
	_, sent := find[SendDraftMsg](keys(t, m, "x", "j"))
	assert.False(t, sent, "only explicit confirmation sends")
	send, ok := find[SendDraftMsg](keys(t, m, "enter"))
	require.True(t, ok)
	assert.Equal(t, SendDraftMsg{Owner: "ws", Revision: 1, Token: "tok-1"}, send)

	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: add.Anchor, Body: add.Body}}, Revision: 1, Status: DraftSending, Preview: "x", Token: "tok-1"})
	_, again := find[SendDraftMsg](keys(t, m, "enter"))
	assert.False(t, again, "sending ignores repeated confirms")

	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: add.Anchor, Body: add.Body}}, Revision: 1, Status: DraftFailed, Err: "agent busy", Preview: "x", Token: "tok-1"})
	assert.Contains(t, ansi.Strip(m.View()), "agent busy")
	_, cancel := find[CancelPreviewMsg](keys(t, m, "esc"))
	assert.True(t, cancel)
}

func TestCommentDraftFlowByMouse(t *testing.T) {
	m := newModel(t, 140, 16)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	g := m.geometry()
	click := func(x, y int, shift bool) []tea.Msg {
		msg := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
		if shift {
			msg.Mod = tea.ModShift
		}
		_, cmd := m.HandleMouse(msg)
		return drain(cmd)
	}
	click(g.readerX+5, g.readerBodyY+2, false)
	click(g.readerX+5, g.readerBodyY+3, true)
	require.GreaterOrEqual(t, m.reader.anchor, 0, "shift+click selects a range")
	assert.Contains(t, ansi.Strip(m.View()), commentLabel)
	click(g.readerX+g.readerW-2, 0, false)
	require.True(t, m.editing, "the heading comment action opens the editor")
	keys(t, m, "o", "k")
	_, ok := find[AddCommentMsg](keys(t, m, "enter"))
	require.True(t, ok)

	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: Anchor{WorktreeID: "w1", Path: "pkg/tui/panel.go", Mode: ModeChanges, Group: GroupUnstaged, NewStart: 2, NewEnd: 2}, Body: "ok"}}, Revision: 2})
	click(g.navX+navPadding+ansi.StringWidth("Changes")+modeGap+1, 0, false)
	require.True(t, m.draftOpen, "the Draft tab is clickable")
	preview, ok := find[PreviewDraftMsg](click(g.navX+navPadding+3, g.navBodyY+1, false))
	require.True(t, ok, "the Send review… row is clickable")
	assert.Equal(t, uint64(2), preview.Revision)

	m.SetDraft(DraftView{Comments: m.draft.Comments, Revision: 2, Status: DraftPreviewing, Target: "◆ s", Preview: "body", Token: "t2"})
	send, ok := find[SendDraftMsg](click(g.readerX+g.readerW-ansi.StringWidth("[Cancel]")-3, 0, false))
	require.True(t, ok, "Send is clickable")
	assert.Equal(t, "t2", send.Token)
	_, cancel := find[CancelPreviewMsg](click(g.readerX+g.readerW-2, 0, false))
	assert.True(t, cancel, "Cancel is clickable")
}

func TestSetSplitPreferencePreservesNarrowFallbackAndCanRestore(t *testing.T) {
	m := openThreeHunks(t, 160, 12, false)
	m.SetSplitPreference(true)
	require.True(t, m.reader.split)
	m.SetSize(70, 10)
	assert.True(t, m.splitPref)
	assert.False(t, m.reader.split, "narrow reader falls back to unified without losing preference")
	m.SetSize(160, 12)
	assert.True(t, m.reader.split, "widening restores desired split layout")
}

func TestRefreshMarksAnchoredCommentStaleInsteadOfReattachingToDifferentLines(t *testing.T) {
	m := openThreeHunks(t, 140, 12, false)
	a, ok := m.reader.anchorFor()
	require.True(t, ok)
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: a, Body: "check this"}}})
	doc := threeHunks(m.doc.Key)
	for i := range doc.Lines {
		if doc.Lines[i].New > 0 {
			doc.Lines[i].New += 100
		}
		if doc.Lines[i].Old > 0 {
			doc.Lines[i].Old += 100
		}
	}
	m.SetDocument(doc)
	assert.True(t, m.staleComments["c1"])
	assert.NotContains(t, ansi.Strip(m.View()), "┊ check this", "a stale comment does not appear under changed lines")
	keys(t, m, "R")
	assert.Contains(t, ansi.Strip(m.View()), "stale")
}

func TestSplitPreferenceSurvivesModelViewStateRestore(t *testing.T) {
	m := openThreeHunks(t, 160, 12, false)
	m.SetSplitPreference(true)
	saved := m.Save()
	restored := New("ws")
	restored.SetSize(70, 12)
	restored.Restore(saved)
	assert.True(t, restored.splitPref)
	assert.False(t, restored.reader.split, "restored preference falls back while narrow")
	restored.SetSize(160, 12)
	restored.SetDocument(threeHunks(saved.Document))
	assert.True(t, restored.reader.split, "restored reader honors saved preference when wide")
}
