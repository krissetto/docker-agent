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

	"github.com/docker/docker-agent/pkg/tui/styles"
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
	copied, _ := find[CopyRequestMsg](keys(t, m, "x"))
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

func TestCommentInputUsesSelectedDiffAnchor(t *testing.T) {
	m := newModel(t, 140, 16)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	m.reader.line = 2
	keys(t, m, "v", "j", "c")
	require.True(t, m.CommentInputActive())
	assert.Equal(t, "docker-agent", m.commentAnchor.RepositoryName)
	assert.Equal(t, "main", m.commentAnchor.Branch)
	assert.Equal(t, []string{`-label := ""`, `+label, hint := "", ""`}, m.commentAnchor.Context)
	assert.Len(t, m.commentAnchor.Version, 16)
	assert.Contains(t, ansi.Strip(m.View()), "Write a review comment")
	assert.Contains(t, m.CommentInputStatus(), "L2")
	keys(t, m, "esc")
	assert.False(t, m.CommentInputActive())
}

func TestBeginRangeSelectionPreservesExistingSelectionAnchor(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	m.reader.anchor = 1
	m.BeginRangeSelection()
	assert.Equal(t, 1, m.reader.anchor)
	assert.Equal(t, 2, m.reader.line)
}

func TestReaderCursorMarkerIsDistinctFromScrollbarAndSelection(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.anchor, m.reader.line = 2, 3
	rows := m.reader.render(m.readerWidth(), m.readerBodyHeight(), true, matcher{})
	assert.Contains(t, rows[m.reader.starts[3]-m.reader.top], styles.InfoStyle.Render("›"), "the active line uses a colored arrow, not a bright vertical bar")
	assert.Contains(t, rows[m.reader.starts[2]-m.reader.top], styles.InfoStyle.Render("▐"), "selected range still uses its distinct range marker")
	assert.NotContains(t, rows[m.reader.starts[3]-m.reader.top], styles.HighlightWhiteStyle.Render("▌"))
}

func TestDiffMouseClickReleaseAndDragOpenInlineComment(t *testing.T) {
	for _, drag := range []bool{false, true} {
		t.Run(map[bool]string{false: "single line", true: "range"}[drag], func(t *testing.T) {
			m := newModel(t, 140, 16)
			selectPanelFile(t, m)
			req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
			m.SetDocument(sampleDocument(req.Key))
			g := m.geometry()
			startY := g.readerBodyY + 2
			_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: startY, Button: tea.MouseLeft})
			if drag {
				_, _ = m.HandleMouse(tea.MouseMotionMsg{X: g.readerX + 16, Y: startY + 1, Button: tea.MouseLeft})
			}
			_, cmd := m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: startY + 1*boolInt(drag), Button: tea.MouseLeft})
			assert.False(t, m.MouseSelecting())
			assert.True(t, m.CommentInputActive(), "mouse release opens the inline editor")
			if drag {
				assert.Len(t, m.commentAnchor.Context, 2)
			} else {
				assert.Len(t, m.commentAnchor.Context, 1)
			}
			assert.NotEmpty(t, drain(cmd))
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestDiffWheelDuringDragExtendsVisibleEndpoint(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	g := m.geometry()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseLeft})
	start := m.reader.line
	_, _ = m.HandleMouse(tea.MouseWheelMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseWheelDown})
	assert.Greater(t, m.reader.line, start, "wheel scrolling while dragging advances the selected endpoint")
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: g.readerBodyY + 3, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.NotEmpty(t, m.commentAnchor.Context)
}

func TestCoalescedWheelPreservesDeltaAndCursorHidesWhenCardOffscreen(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	doc := longDocument(m.doc.Key, 120)
	m.SetDocument(doc)
	m.reader.line = 0
	g := m.geometry()
	m.HandleWheelDelta(m.x+g.readerX+16, m.y+g.readerBodyY+2, 4)
	assert.Equal(t, 8, m.reader.top, "four accumulated ticks move eight visual rows")

	m.reader.line = 2
	m.reader.anchor = -1
	m.startComment()
	require.NotNil(t, m.Cursor(), "visible inline input exposes its cursor")
	m.reader.top = m.reader.total - 1
	m.syncScrollviews()
	assert.Nil(t, m.Cursor(), "cursor is hidden when its card is scrolled offscreen")
}

func TestWheelDeltaMovesViewportWithoutForcingCursorAndOffscreenInputCursorHides(t *testing.T) {
	m := openThreeHunks(t, 140, 12, false)
	doc := longDocument(m.doc.Key, 120)
	m.SetDocument(doc)
	m.reader.line = 0
	g := m.geometry()
	m.HandleWheelDelta(m.x+g.readerX+16, m.y+g.readerBodyY+2, 4)
	assert.Equal(t, 8, m.reader.top, "four accumulated wheel ticks move by eight rows")
	assert.Positive(t, m.reader.line, "reader scroll moves cursor into the newly visible range")

	// Open a comment near the end, then wheel it off-screen. The reader can
	// keep scrolling; the root must not retain a hardware cursor in another row.
	m.reader.line = 2
	m.startComment()
	require.True(t, m.CommentInputActive())
	require.NotNil(t, m.Cursor(), "the visible editor exposes its cursor")
	for range 100 {
		m.HandleWheelDelta(m.x+g.readerX+16, m.y+g.readerBodyY+2, 1)
	}
	assert.Nil(t, m.Cursor(), "the hardware cursor is hidden when the inline editor is outside the viewport")
}

func TestCommentEditorRetargetsOnlyWhileEmpty(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	g := m.geometry()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	item := m.reader.lineItem[3]
	m.reader.layout(m.readerWidth())
	otherY := g.readerBodyY + m.reader.starts[item] - m.reader.top
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: otherY, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: otherY, Button: tea.MouseLeft})
	assert.True(t, m.CommentInputActive())
	assert.NotEqual(t, 0, m.commentAnchor.NewStart)
	_, _ = m.HandleKey(press("a"))
	oldAnchor := m.commentAnchor
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: g.readerBodyY + 4, Button: tea.MouseLeft})
	assert.Equal(t, oldAnchor, m.commentAnchor, "typed comments cannot be relocated by a line click")
	_, cmd := m.HandleKey(press("esc"))
	assert.Nil(t, cmd)
	assert.False(t, m.CommentInputActive())
}

func TestEmptyCommentCancelsOnDocumentSwitchButTypedCommentBlocks(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	oldKey := m.doc.Key
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	newKey := DocumentKey{Mode: oldKey.Mode, WorktreeID: oldKey.WorktreeID, Path: "other.go", Group: oldKey.Group}
	cmd := m.OpenDocument(newKey)
	assert.Nil(t, cmd, "a pending inline editor is never silently discarded")
	assert.True(t, m.CommentInputActive())
	assert.Equal(t, oldKey, m.requested)
	assert.Contains(t, m.hint, "Save or cancel")
	_, _ = m.HandleKey(press("esc"))

	m = openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	_, _ = m.HandleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	oldKey = m.doc.Key
	newKey.Path = "other.go"
	cmd = m.OpenDocument(newKey)
	assert.Nil(t, cmd, "typed text cannot be silently moved to a different document")
	assert.Equal(t, oldKey, m.requested)
	assert.True(t, m.CommentInputActive())
	assert.Contains(t, m.hint, "Save or cancel")
}

func TestCommentEditorRemapsAnchorAfterSameDocumentReload(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	anchor := m.commentAnchor
	oldItem := m.commentItem
	doc := threeHunks(m.doc.Key)
	doc.Lines = append(doc.Lines[:1], append([]Line{{Kind: LineContext, Old: 1, New: 1, Text: "inserted"}}, doc.Lines[1:]...)...)
	m.SetDocument(doc)
	item, ok := m.reader.commentItem(anchor)
	require.True(t, ok)
	assert.True(t, m.CommentInputActive())
	assert.Equal(t, item, m.commentItem)
	assert.NotEqual(t, oldItem, m.commentItem, "editor follows its line after a hunk is inserted before it")
}

func TestClickingNavigatorCancelsEmptyEditor(t *testing.T) {
	m := newModel(t, 140, 16)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	g := m.geometry()
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.navX + 5, Y: g.navBodyY + 2, Button: tea.MouseLeft})
	assert.False(t, m.CommentInputActive(), "empty editor cancels before the navigator takes focus")
}

func TestUnifiedFirstContentCellCanStartASelection(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.doc.Lines = []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "first"}}
	m.SetDocument(m.doc)
	g := m.geometry()
	x := g.readerX + m.reader.gutter(m.readerWidth()).width()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: x, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	assert.True(t, m.MouseSelecting(), "the first text column after the gutter is live")
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: x, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.Equal(t, []string{" first"}, m.commentAnchor.Context)
}

func TestExactTextSelectionAcrossWrappedLinesAndUnicode(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	key := m.doc.Key
	doc := Document{Key: key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "A	界éZ"}, {Kind: LineContext, Old: 2, New: 2, Text: "second line"}}}
	m.SetDocument(doc)
	g := m.geometry()
	row := g.readerBodyY
	// Start inside A and end after the multi-cell grapheme. The source columns
	// remain rune offsets even though screen coordinates are display cells.
	startX := g.readerX + m.reader.gutter(m.readerWidth()).width() + 2
	endX := g.readerX + m.reader.gutter(m.readerWidth()).width() + 8
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: startX, Y: row + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: endX, Y: row + 1, Button: tea.MouseLeft})
	_, cmd := m.HandleMouse(tea.MouseReleaseMsg{X: endX, Y: row + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	a := m.commentAnchor
	assert.True(t, a.CharacterRange)
	assert.Equal(t, 1, a.StartColumn)
	assert.Equal(t, 6, a.EndColumn)
	assert.Equal(t, []string{" \t界éZ"}, a.Context)
	assert.NotEmpty(t, drain(cmd))
	_ = row
}

func TestWrappedMouseSelectionUsesVisualRowsAndCopyKeepsExactSlice(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	text := strings.Repeat("a", m.reader.contentWidth(m.readerWidth())*2+5)
	doc := Document{Key: m.doc.Key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: text}}}
	m.SetDocument(doc)
	item := m.reader.lineItem[1]
	m.reader.layout(m.readerWidth())
	start := m.reader.starts[item]
	width := m.reader.contentWidth(m.readerWidth())
	g := m.geometry()
	x1 := g.readerX + m.reader.gutter(m.readerWidth()).width() + width - 1
	x2 := g.readerX + m.reader.gutter(m.readerWidth()).width() + 2
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: x1, Y: g.readerBodyY + start, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: x2, Y: g.readerBodyY + start + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: x2, Y: g.readerBodyY + start + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.Equal(t, []string{" " + text[width-1:width+3]}, m.commentAnchor.Context)
	assert.Equal(t, width-1, m.commentAnchor.StartColumn)
	assert.Equal(t, width+3, m.commentAnchor.EndColumn)
	assert.Contains(t, strings.Join(m.reader.render(m.readerWidth(), m.reader.total, true, matcher{}), "\n"), text[width-1:width+2], "the exact range is visible across the wrap edge")
}

func TestTabColumnHitTestingUsesVisualTabStopAfterPrefix(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	doc := Document{Key: m.doc.Key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "x\ty"}}}
	m.SetDocument(doc)
	g := m.geometry()
	row := ansi.Strip(strings.Split(m.View(), "\n")[g.readerBodyY+1])
	xpos := strings.Index(row, "x")
	tabAt := ansi.StringWidth(row[:xpos]) + 1
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: tabAt, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: tabAt + 2, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: tabAt + 2, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.Equal(t, []string{" \t"}, m.commentAnchor.Context)
	assert.Equal(t, 1, m.commentAnchor.StartColumn)
	assert.Equal(t, 2, m.commentAnchor.EndColumn)
}

func TestReverseDragHasOrderedExclusiveUnicodeOffsets(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	doc := Document{Key: m.doc.Key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "abc界xyz"}}}
	m.SetDocument(doc)
	g := m.geometry()
	base := g.readerX + m.reader.gutter(m.readerWidth()).width()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: base + 6, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: base + 2, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: base + 2, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	a := m.commentAnchor
	assert.Equal(t, 2, a.StartColumn)
	assert.Equal(t, 6, a.EndColumn)
	assert.Equal(t, []string{" c界xy"}, a.Context)
}

func TestMultilineDragSelectionUsesPartialEndpointsAndFullMiddleLine(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	doc := Document{Key: m.doc.Key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "alpha"}, {Kind: LineContext, Old: 2, New: 2, Text: "middle"}, {Kind: LineContext, Old: 3, New: 3, Text: "omega"}}}
	m.SetDocument(doc)
	g := m.geometry()
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	startLine, endLine := rows[g.readerBodyY+1], rows[g.readerBodyY+3]
	startOffset := ansi.StringWidth(startLine[:strings.Index(startLine, "alpha")]) + 1
	endOffset := ansi.StringWidth(endLine[:strings.Index(endLine, "omega")]) + 1
	startX := g.readerX + startOffset
	endX := g.readerX + endOffset
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: startX, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: endX, Y: g.readerBodyY + 3, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: endX, Y: g.readerBodyY + 3, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.Equal(t, []string{" lpha", " middle", " om"}, m.commentAnchor.Context)
	assert.Equal(t, 1, m.commentAnchor.StartColumn)
	assert.Equal(t, 2, m.commentAnchor.EndColumn)
}

func TestSplitMouseSelectionUsesAfterPaneOnly(t *testing.T) {
	m := openThreeHunks(t, 180, 16, true)
	// Select one paired replacement. A click in the removed pane is ignored.
	item := -1
	for i, it := range m.reader.items {
		if it.left >= 0 && it.right >= 0 && it.left != it.right {
			item = i
			break
		}
	}
	require.GreaterOrEqual(t, item, 0)
	m.reader.line = item
	m.ensureLineVisible()
	g := m.geometry()
	side, _ := m.reader.splitGeometry(m.readerWidth())
	y := g.readerBodyY + m.reader.starts[item] - m.reader.top
	leftX := g.readerX + 4
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: leftX, Y: y, Button: tea.MouseLeft})
	assert.False(t, m.MouseSelecting())
	rightX := g.readerX + side + m.reader.digits + 6
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: rightX, Y: y, Button: tea.MouseLeft})
	require.True(t, m.MouseSelecting())
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: rightX, Y: y, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.Positive(t, m.commentAnchor.NewStart)
	assert.Equal(t, 0, m.commentAnchor.OldStart)
}

func TestReaderTitleActionsExposeAndHitWholeFileAndEditor(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	assert.Contains(t, plain(m), "[f File]")
	width := m.geometry().readerW
	assert.Equal(t, "counterpart", m.readerTitleAction(width-1))
	key := m.doc.Key
	key.Mode, key.Group = ModeFiles, GroupNone
	_ = m.SetMode(ModeFiles)
	m.OpenDocument(key)
	m.SetDocument(Document{Key: key, State: StateReady, Lines: []Line{{Kind: LineContext, New: 1, Text: "line"}}})
	assert.Contains(t, plain(m), "[f Diff] [o Edit]")
	assert.Equal(t, "editor", m.readerTitleAction(width-1))
	assert.Equal(t, "counterpart", m.readerTitleAction(width-ansi.StringWidth("[o Edit]")-4))
	_, cmd := m.HandleKey(press("o"))
	_, ok := find[OpenExternalEditorRequestMsg](drain(cmd))
	assert.True(t, ok)
	m.mode = ModeChanges
	_, cmd = m.HandleKey(press("o"))
	assert.Contains(t, m.hint, "then o Edit")
	assert.Nil(t, cmd)
}

func TestEmptyFileIndicatorRendersWithoutSyntheticText(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	key := DocumentKey{Mode: ModeFiles, WorktreeID: "w1", Path: "new.txt"}
	req := m.OpenDocument(key)
	require.NotNil(t, req)
	m.SetDocument(Document{Key: key, State: StateReady, Indicators: []DocumentIndicator{{Line: 0, Kind: ChangeAdded}}})
	assert.Contains(t, plain(m), "new file")
	assert.Empty(t, m.doc.Lines, "indicators do not alter file source")
}

func TestWholeFileIndicatorsAndCounterpartRequest(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 3
	_, cmd := m.HandleKey(press("f"))
	msg, ok := find[CounterpartRequestMsg](drain(cmd))
	require.True(t, ok)
	assert.Equal(t, ModeFiles, msg.Key.Mode)
	assert.Equal(t, 3, msg.NewLine)
	require.NotNil(t, m.OpenCounterpart(msg.Key, msg.OldLine, msg.NewLine, msg.Top))
	whole := Document{Key: msg.Key, State: StateReady, Lines: []Line{{Kind: LineContext, New: 1, Text: "one"}, {Kind: LineContext, New: 2, Text: "two"}, {Kind: LineContext, New: 3, Text: "three"}}, Indicators: []DocumentIndicator{{Line: 2, Kind: ChangeModified}}}
	m.SetDocument(whole)
	assert.Equal(t, 2, m.reader.line, "line mapping follows the selected target line")
	assert.Contains(t, plain(m), "~")
	_, editorCmd := m.HandleKey(press("o"))
	external, ok := find[OpenExternalEditorRequestMsg](drain(editorCmd))
	require.True(t, ok)
	assert.Equal(t, msg.Key, external.Key)
}

func TestInlineEditorCardStartsAfterExpandedCommentsAtSameAnchor(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	a, ok := m.reader.anchorFor()
	require.True(t, ok)
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: a, Body: "first\nbody"}, {ID: "c2", Anchor: a, Body: "second\nbody"}}, Revision: 1})
	m.commentExpanded["c1"], m.commentExpanded["c2"] = true, true
	m.syncReaderComments()
	_, cmd := m.HandleKey(press("c"))
	_ = cmd
	require.True(t, m.CommentInputActive())
	m.reader.layout(m.readerWidth())
	want := m.reader.starts[m.commentItem] + m.reader.textRows(m.commentItem, m.readerWidth()) + m.reader.commentRows(m.commentItem)
	assert.Equal(t, want, m.commentCardTop())
}

func TestSplitRemovedOnlyPatchSelectionCopiesItsHunk(t *testing.T) {
	m := openThreeHunks(t, 180, 16, true)
	m.doc.Lines = []Line{{Kind: LineHunk, Text: "@@ -1 +1 @@"}, {Kind: LineRemove, Old: 1, Text: "old"}}
	m.reader.setDocument(&m.doc, false)
	removed := 0
	m.reader.anchor, m.reader.line = removed, removed
	assert.NotPanics(t, func() {
		patch, n := m.reader.copyPatch(false)
		assert.Equal(t, 1, n)
		assert.Contains(t, patch, "-old")
	})
}

func TestExpandedCommentRowsHaveFullHitboxWithoutSelectingCode(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	a, ok := m.reader.anchorFor()
	require.True(t, ok)
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: a, Body: "line one\nline two"}}, Revision: 1})
	commentItem, ok := m.reader.commentItem(a)
	require.True(t, ok)
	m.reader.layout(m.readerWidth())
	card := m.reader.starts[commentItem] + m.reader.textRows(commentItem, m.readerWidth())
	g := m.geometry()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + g.readerW - 2, Y: g.readerBodyY + card - m.reader.top, Button: tea.MouseLeft})
	assert.True(t, m.commentExpanded["c1"])
	m.reader.layout(m.readerWidth())
	assert.Greater(t, m.reader.commentRows(commentItem), 1)
	rows := m.reader.render(m.readerWidth(), m.reader.total, true, matcher{})
	assert.Contains(t, strings.Join(rows, "\n"), "line two")
	assert.False(t, m.MouseSelecting())
}

func TestGeneralOnlyDraftCanPreviewAndIsVisible(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.SetDraft(DraftView{General: "Please review this change", Revision: 2})
	assert.Contains(t, plain(m), "Draft 1")
	_, cmd := m.HandleKey(press("alt+c"))
	_, ok := find[GeneralCommentRequestMsg](drain(cmd))
	assert.True(t, ok)
	m.draftOpen = true
	m.area = areaNavigator
	m.draftCursor = 0
	_, cmd = m.HandleKey(press("enter"))
	_, ok = find[GeneralCommentRequestMsg](drain(cmd))
	assert.True(t, ok)
	m.draft.General = ""
	m.draft.Comments = []Comment{{ID: "c1", Anchor: Anchor{Path: "a.go"}, Body: "comment"}}
	_, cmd = m.HandleKey(press("s"))
	_, ok = find[PreviewDraftMsg](drain(cmd))
	assert.True(t, ok)
}

func TestCommentShortcutDuringMouseHoldEndsDragCapture(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	g := m.geometry()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseLeft})
	require.True(t, m.MouseSelecting())
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	assert.False(t, m.MouseSelecting())
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: g.readerBodyY + 2, Button: tea.MouseLeft})
	assert.False(t, m.MouseSelecting())
	assert.True(t, m.CommentInputActive())
}

func TestClickingCommentPreviewCannotStartLineSelection(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	anchor, ok := m.reader.anchorFor()
	require.True(t, ok)
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: anchor, Body: "existing review comment"}}, Revision: 1})
	m.reader.layout(m.readerWidth())
	commentRow := m.reader.starts[m.reader.line] + m.reader.textRows(m.reader.line, m.readerWidth())
	g := m.geometry()
	y := g.readerBodyY + commentRow - m.reader.top
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: g.readerX + 16, Y: y, Button: tea.MouseLeft})
	assert.False(t, m.MouseSelecting(), "a preview row is not a diff line start")
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: g.readerX + 16, Y: g.readerBodyY + 3, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: g.readerX + 16, Y: g.readerBodyY + 3, Button: tea.MouseLeft})
	assert.False(t, m.CommentInputActive(), "dragging away from a preview must not open a new inline editor")
}

func TestSubmittingEmptyCommentCancelsEditor(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	_, cmd := m.HandleKey(press("enter"))
	assert.Empty(t, drain(cmd), "empty submission is treated as cancellation")
	assert.False(t, m.CommentInputActive())
}

func TestCharacterSelectionDoesNotLeakAfterEscapeAndKeyboardNavigation(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	key := m.doc.Key
	doc := Document{Key: key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 1, New: 1, Text: "first-line"}, {Kind: LineContext, Old: 2, New: 2, Text: "second-line"}}}
	m.SetDocument(doc)
	g := m.geometry()
	base := g.readerX + m.reader.gutter(m.readerWidth()).width()
	_, _ = m.HandleMouse(tea.MouseClickMsg{X: base + 2, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseMotionMsg{X: base + 5, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	_, _ = m.HandleMouse(tea.MouseReleaseMsg{X: base + 5, Y: g.readerBodyY + 1, Button: tea.MouseLeft})
	require.True(t, m.CommentInputActive())
	assert.True(t, m.commentAnchor.CharacterRange)
	_, _ = m.HandleKey(press("esc"))
	assert.Nil(t, m.reader.charSelection)
	assert.Equal(t, -1, m.reader.anchor)
	_, _ = m.HandleKey(press("down"))
	assert.Nil(t, m.reader.charSelection)
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	assert.False(t, m.commentAnchor.CharacterRange)
	assert.Equal(t, []string{" second-line"}, m.commentAnchor.Context)
}

func TestRejectedCommentSavePreservesEditorAndIgnoresStaleAcknowledgement(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	for _, r := range "keep me" {
		_, _ = m.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	anchor := m.commentAnchor
	_, cmd := m.HandleKey(press("enter"))
	req, ok := find[AddCommentMsg](drain(cmd))
	require.True(t, ok)
	require.NotEmpty(t, req.RequestID)
	assert.True(t, m.commentSaving)
	assert.True(t, m.CommentInputActive())
	assert.False(t, m.HandleCommentResult(CommentResultMsg{Owner: m.owner, RequestID: "stale", Err: "old failure"}))
	assert.Equal(t, "keep me", m.commentInput.Value())
	assert.True(t, m.HandleCommentResult(CommentResultMsg{Owner: m.owner, RequestID: req.RequestID, Err: "validation rejected"}))
	assert.True(t, m.CommentInputActive())
	assert.False(t, m.commentSaving)
	assert.Equal(t, "keep me", m.commentInput.Value())
	assert.Equal(t, anchor, m.commentAnchor)
	assert.Contains(t, m.hint, "validation rejected")
	_, retryCmd := m.HandleKey(press("enter"))
	retry, ok := find[AddCommentMsg](drain(retryCmd))
	require.True(t, ok)
	assert.NotEqual(t, req.RequestID, retry.RequestID)
	assert.True(t, m.HandleCommentResult(CommentResultMsg{Owner: m.owner, RequestID: retry.RequestID}))
	assert.False(t, m.CommentInputActive())
	assert.Nil(t, m.reader.charSelection)
	assert.False(t, m.HandleCommentResult(CommentResultMsg{Owner: m.owner, RequestID: retry.RequestID}), "duplicate ACKs cannot close a later editor")
}

func TestMultilineReverseCharacterAnchorKeepsIndependentEndpointColumns(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	doc := Document{Key: m.doc.Key, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@"}, {Kind: LineContext, Old: 10, New: 10, Text: "ABCDEFGHIJKLMN"}, {Kind: LineContext, Old: 11, New: 11, Text: "abcdefghijklmnop"}}}
	m.SetDocument(doc)
	first, last := m.reader.lineItem[1], m.reader.lineItem[2]
	m.reader.anchor, m.reader.line = first, last
	m.reader.charSelection = &readerCharSelection{start: textPoint{line: 2, column: 10}, end: textPoint{line: 1, column: 2}}
	a, ok := m.reader.anchorFor()
	require.True(t, ok)
	assert.True(t, a.CharacterRange)
	assert.Equal(t, 2, a.StartColumn)
	assert.Equal(t, 10, a.EndColumn)
	assert.Equal(t, []string{" CDEFGHIJKLMN", " abcdefghij"}, a.Context)
}

func TestInlineCommentSubmitAndEditUseDraftMessages(t *testing.T) {
	m := openThreeHunks(t, 140, 16, false)
	m.reader.line = 2
	_, _ = m.HandleKey(press("c"))
	require.True(t, m.CommentInputActive())
	for _, r := range "note" {
		_, _ = m.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	_, cmd := m.HandleKey(press("enter"))
	add, ok := find[AddCommentMsg](drain(cmd))
	require.True(t, ok)
	assert.Equal(t, "note", add.Body)
	assert.True(t, m.CommentInputActive(), "save stays in the inline form until host ACK")
	require.True(t, m.HandleCommentResult(CommentResultMsg{Owner: m.owner, RequestID: add.RequestID}))
	m.SetDraft(DraftView{Comments: []Comment{{ID: "c1", Anchor: add.Anchor, Body: add.Body}}, Revision: 1})
	_, cmd = m.HandleKey(press("R"))
	_ = cmd
	_, cmd = m.HandleKey(press("e"))
	assert.NotNil(t, cmd, "the status message is published while opening the edit form")
	assert.True(t, m.CommentInputActive())
	assert.Equal(t, "note", m.commentInput.Value())
	_, cmd = m.HandleKey(press("esc"))
	assert.Nil(t, cmd)
	assert.False(t, m.CommentInputActive())
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

func TestReaderSetSplitWhileDocumentHasNoLines(t *testing.T) {
	var r readerState
	empty := Document{State: StateLoading}
	r.setDocument(&empty, false)
	assert.NotPanics(t, func() { r.setSplit(true) })
	assert.True(t, r.split)
	assert.Empty(t, r.items)
	assert.Equal(t, 0, r.line)

	r.split = false
	r.anchor = 0 // stale selection from the document that was replaced
	assert.NotPanics(t, func() { r.setSplit(true) })
	assert.Equal(t, -1, r.anchor, "an unmappable active selection must be cleared")
}
