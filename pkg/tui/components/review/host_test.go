package review

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeHost answers review requests the way the root adapter does, so whole
// workflows run through the public contract only.
type fakeHost struct {
	t        *testing.T
	m        *Model
	snapshot Snapshot
	docs     map[DocumentKey]Document
	dirs     map[dirID]Directory
	copies   []CopyRequestMsg
	unowned  []string
	closed   bool
	opened   []DocumentKey
}

func newFakeHost(t *testing.T, w int) *fakeHost {
	t.Helper()
	host := &fakeHost{t: t, m: New("ws"), snapshot: sampleSnapshot(), docs: map[DocumentKey]Document{}, dirs: map[dirID]Directory{}}
	panel := DocumentKey{Mode: ModeChanges, WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: GroupUnstaged}
	host.docs[panel] = sampleDocument(panel)
	tabs := DocumentKey{Mode: ModeChanges, WorktreeID: "w1", Path: "pkg/tui/tabs.go", Group: GroupStaged}
	host.docs[tabs] = Document{Key: tabs, State: StateReady, Lines: []Line{{Kind: LineHunk, Text: "@@ -1 +1 @@"}, {Kind: LineAdd, New: 1, Text: "tabs"}}}
	host.dirs[dirID{"w1", ""}] = Directory{WorktreeID: "w1", State: StateReady, Entries: []Entry{{Name: "pkg", Dir: true}, {Name: "go.mod"}}}
	host.dirs[dirID{"w1", "pkg"}] = Directory{WorktreeID: "w1", Path: "pkg", State: StateReady, Entries: []Entry{{Name: "main.go"}}}
	gomod := DocumentKey{Mode: ModeFiles, WorktreeID: "w1", Path: "go.mod"}
	host.docs[gomod] = Document{Key: gomod, State: StateReady, Lines: []Line{{New: 1, Text: "module example"}, {New: 2, Text: "go 1.27"}}}
	host.m.SetSize(w, 20)
	host.m.SetSession(Session{Title: "main session", WorktreeID: "w1"})
	host.dispatch(host.m.SetSnapshot(host.snapshot))
	host.m.Focus()
	expandAll(host.m)
	return host
}

func (h *fakeHost) dispatch(cmd tea.Cmd) {
	for _, msg := range drain(cmd) {
		switch msg := msg.(type) {
		case OpenDocumentRequestMsg:
			h.opened = append(h.opened, msg.Key)
			doc, ok := h.docs[msg.Key]
			if !ok {
				doc = Document{Key: msg.Key, State: StateError, Err: "missing"}
			}
			h.m.SetDocument(doc)
		case LoadDirectoryRequestMsg:
			if d, ok := h.dirs[dirID{msg.WorktreeID, msg.Path}]; ok {
				h.dispatch(h.m.SetDirectory(d))
			}
		case LoadWorktreeRequestMsg, RefreshRequestMsg:
			h.snapshot.Generation++
			h.dispatch(h.m.SetSnapshot(h.snapshot))
		case CopyRequestMsg:
			h.copies = append(h.copies, msg)
		case CloseRequestMsg:
			h.closed = true
		case SelectionChangedMsg, FocusRequestMsg:
		default:
			h.t.Fatalf("unexpected message %T", msg)
		}
	}
}

func (h *fakeHost) press(seq ...string) {
	for _, k := range seq {
		handled, cmd := h.m.HandleKey(press(k))
		if !handled {
			h.unowned = append(h.unowned, k)
		}
		h.dispatch(cmd)
	}
}

func (h *fakeHost) lastCopy() CopyRequestMsg {
	h.t.Helper()
	require.NotEmpty(h.t, h.copies)
	return h.copies[len(h.copies)-1]
}

func TestKeyboardOnlyReviewWorkflow(t *testing.T) {
	for _, width := range []int{140, 72} {
		h := newFakeHost(t, width)
		// Down to the staged file, open it, step to the next file from the reader.
		h.press("j", "j", "j", "j", "enter")
		require.True(t, h.m.hasDoc)
		assert.Equal(t, "pkg/tui/tabs.go", h.m.doc.Key.Path)
		h.press("J")
		assert.Equal(t, "pkg/tui/panel.go", h.m.doc.Key.Path, "J moves to the next file without leaving the reader")
		assert.Equal(t, areaReader, h.m.area)

		h.press("/", "l", "a", "b", "e", "l", "enter", "n", "g", "v", "G", "x")
		copied := h.lastCopy()
		assert.Equal(t, CopyText, copied.Kind)
		assert.Equal(t, 7, strings.Count(copied.Text, "\n"), "full keyboard selection copies every line")
		assert.Contains(t, copied.Text, "\n-label := \"\"\n+label, hint")

		h.press("esc", "esc", "esc")
		assert.Equal(t, areaNavigator, h.m.area)

		h.dispatch(h.m.SetMode(ModeFiles))
		assert.Equal(t, ModeFiles, h.m.Mode())
		assert.Contains(t, plain(h.m), "go.mod")
		h.press("j", "j", "j", "enter")
		assert.Equal(t, "go.mod", h.m.doc.Key.Path)
		h.press("x")
		assert.Equal(t, "module example\n", h.lastCopy().Text)

		h.press("esc", "q")
		assert.True(t, h.closed)
		assert.Empty(t, h.unowned, "every review key is owned by the component")
	}
}

func TestFilesFolderExpansionThroughHost(t *testing.T) {
	h := newFakeHost(t, 120)
	h.dispatch(h.m.SetMode(ModeFiles))
	h.press("j", "j", "enter")
	view := plain(h.m)
	assert.Contains(t, view, "pkg/")
	assert.Contains(t, view, "main.go")
	h.press("left")
	assert.NotContains(t, plain(h.m), "main.go")
}

func TestRefreshKeepsSelectionAndPosition(t *testing.T) {
	h := newFakeHost(t, 140)
	selectPanelFile(t, h.m)
	h.press("enter", "j", "j")
	line := h.m.reader.line
	h.press("r")
	r, ok := h.m.selectedRow()
	require.True(t, ok)
	assert.Equal(t, "pkg/tui/panel.go", r.path)
	assert.Equal(t, line, h.m.reader.line)
}

func TestViewIsPure(t *testing.T) {
	h := newFakeHost(t, 140)
	selectPanelFile(t, h.m)
	h.press("enter", "/", "l", "a")
	before := h.m.Save()
	first := h.m.View()
	for range 3 {
		assert.Equal(t, first, h.m.View())
	}
	assert.Equal(t, before, h.m.Save())
}

func TestUntrustedTextIsSanitizedForDisplayButCopiedRaw(t *testing.T) {
	h := newFakeHost(t, 140)
	evil := "pkg/\x1b]52;c;ZXZpbA==\x07evil\u202egnp.go"
	h.snapshot.Repositories[0].Worktrees[0].Files = []FileChange{{Path: evil, Group: GroupUnstaged, Status: "M\x1b[31m"}}
	key := DocumentKey{Mode: ModeChanges, WorktreeID: "w1", Path: evil, Group: GroupUnstaged}
	h.docs[key] = Document{Key: key, State: StateReady, Lines: []Line{{Kind: LineAdd, New: 1, Text: "x\x1b[2Jy\tz"}}}
	h.dispatch(h.m.SetSnapshot(h.snapshot))
	for i, r := range h.m.currentRows() {
		if r.kind == rowFile {
			h.m.cursor = i
			h.m.rememberCursor()
		}
	}
	h.press("enter")
	view := h.m.View()
	assert.NotContains(t, view, "\x1b]52")
	assert.NotContains(t, view, "\x1b[2J")
	assert.NotContains(t, view, "\x07")
	assert.NotContains(t, view, "\u202e")

	h.press("x", "y")
	require.Len(t, h.copies, 2)
	assert.Equal(t, "+x\x1b[2Jy\tz\n", h.copies[0].Text, "copies keep source bytes")
	assert.Equal(t, evil, h.copies[1].Text)
}

func TestNarrowDockStaysSinglePane(t *testing.T) {
	h := newFakeHost(t, 140)
	selectPanelFile(t, h.m)
	h.press("enter")
	assert.True(t, h.m.geometry().twoColumn)
	h.m.SetSize(56, 20)
	assertFrame(t, h.m.View(), 56, 20)
	assert.False(t, h.m.geometry().twoColumn)
	assert.Contains(t, plain(h.m), "Unstaged · hunk 1/2", "reader keeps focus when the dock narrows")
	h.press("esc")
	assert.Contains(t, plain(h.m), "Changes  Draft 0")
}

func TestHelpFollowsContext(t *testing.T) {
	h := newFakeHost(t, 140)
	ctx, sections := h.m.Help()
	assert.Equal(t, "Changes review — navigator", ctx)
	require.NotEmpty(t, sections)
	selectPanelFile(t, h.m)
	h.press("enter")
	ctx, _ = h.m.Help()
	assert.Equal(t, "Changes review — reader", ctx)
	h.press("/")
	ctx, _ = h.m.Help()
	assert.Equal(t, "Changes review — typing search", ctx)
}

func TestClearDirectoriesInvalidatesCacheWithoutSchedulingLoads(t *testing.T) {
	m := New("ws")
	m.SetSize(100, 20)
	m.mode = ModeFiles
	m.dirs[dirID{worktree: "w1", path: ""}] = Directory{WorktreeID: "w1", State: StateReady, Entries: []Entry{{Name: "old.go"}}}
	m.loadPending[dirKey("w1", "")] = true
	m.loadPending[worktreeKey("w1")] = true
	m.ClearDirectories()
	assert.Empty(t, m.dirs)
	assert.NotContains(t, m.loadPending, dirKey("w1", ""))
	assert.Contains(t, m.loadPending, worktreeKey("w1"), "directory cache reset should preserve unrelated request guards")
	assert.Empty(t, m.dirs, "clearing cache is inert and cannot schedule replacement loads")
}
