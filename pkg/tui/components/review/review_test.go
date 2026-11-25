package review

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func press(s string) tea.KeyPressMsg {
	named := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "space": tea.KeySpace,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	}
	var mod tea.KeyMod
	if rest, ok := strings.CutPrefix(s, "ctrl+"); ok {
		mod, s = tea.ModCtrl, rest
	} else if rest, ok := strings.CutPrefix(s, "shift+"); ok {
		mod, s = tea.ModShift, rest
	}
	if code, ok := named[s]; ok {
		return tea.KeyPressMsg{Code: code, Mod: mod}
	}
	r, _ := utf8.DecodeRuneInString(s)
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	return tea.KeyPressMsg{Code: r, Text: s}
}

// drain runs a command tree and returns every non-nil message.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	switch msg := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range msg {
			out = append(out, drain(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func find[T any](msgs []tea.Msg) (T, bool) {
	for _, msg := range msgs {
		if v, ok := msg.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func keys(t *testing.T, m *Model, seq ...string) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	for _, k := range seq {
		_, cmd := m.HandleKey(press(k))
		out = append(out, drain(cmd)...)
	}
	return out
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		Generation: 1,
		CapturedAt: time.Date(2025, 1, 1, 14, 2, 0, 0, time.UTC),
		Scope:      Scope{Label: "~/dev/ai-dev/docker-agent"},
		Repositories: []Repository{
			{ID: "r1", Name: "docker-agent", HiddenOutside: 2, Worktrees: []Worktree{
				{ID: "w1", Path: "~/dev/ai-dev/docker-agent", Branch: "main", Current: true, State: StateReady, Files: []FileChange{
					{Path: "pkg/tui/tabs.go", Group: GroupStaged, Status: "M", Added: 4, Removed: 1},
					{Path: "pkg/tui/panel.go", Group: GroupUnstaged, Status: "M", Added: 18, Removed: 6},
					{Path: "pkg/tui/tui.go", Group: GroupConflicts, Status: "U"},
					{Path: "notes.md", Group: GroupUntracked, Status: "?"},
				}},
				{ID: "w2", Path: "./wt/shell", Branch: "shell-wt", Linked: true, State: StateUnscanned},
			}},
			{ID: "r2", Name: "kit/vendor/foo", Kind: RepoNested, Worktrees: []Worktree{
				{ID: "w3", Path: "kit/vendor/foo", Detached: "3e336a8", State: StateReady},
			}},
		},
	}
}

func sampleDocument(key DocumentKey) Document {
	return Document{Key: key, State: StateReady, PatchHeader: "diff --git a/pkg/tui/panel.go b/pkg/tui/panel.go\n--- a/pkg/tui/panel.go\n+++ b/pkg/tui/panel.go", Lines: []Line{
		{Kind: LineHunk, Text: "@@ -1,3 +1,3 @@ func a()"},
		{Kind: LineContext, Old: 1, New: 1, Text: "package tui"},
		{Kind: LineRemove, Old: 2, Text: "label := \"\""},
		{Kind: LineAdd, New: 2, Text: "label, hint := \"\", \"\""},
		{Kind: LineHunk, Text: "@@ -40,2 +40,3 @@ func b()"},
		{Kind: LineContext, Old: 40, New: 40, Text: "return label"},
		{Kind: LineAdd, New: 41, Text: "// label again"},
	}}
}

func newModel(t *testing.T, w, h int) *Model {
	t.Helper()
	m := New("ws")
	m.SetSize(w, h)
	m.SetSession(Session{Title: "main session", WorktreeID: "w1"})
	drain(m.SetSnapshot(sampleSnapshot()))
	m.Focus()
	expandAll(m)
	return m
}

// expandAll opens every structural row so tests can reach any file directly.
func expandAll(m *Model) {
	for changed := true; changed; {
		changed = false
		for _, r := range m.currentRows() {
			if r.structural() && !r.expanded {
				m.expanded[r.key] = true
				changed = true
			}
		}
		m.invalidateRows()
	}
	m.syncScrollviews()
}

func TestReviewCursorInputFollowsKeyboardCapture(t *testing.T) {
	m := newModel(t, 80, 20)
	require.Nil(t, m.CursorInput())
	m.startFilter()
	require.Same(t, &m.filter, m.CursorInput())
	m.Blur()
	require.Nil(t, m.CursorInput())
	m.Focus()
	m.filterActive = false
	m.searchOn = true
	m.search.Focus()
	require.Same(t, &m.search, m.CursorInput())
}

func assertFrame(t *testing.T, view string, w, h int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	require.Len(t, lines, h)
	for i, l := range lines {
		assert.Equal(t, w, ansi.StringWidth(l), "row %d: %q", i, ansi.Strip(l))
	}
}

func plain(m *Model) string { return ansi.Strip(m.View()) }

func selectPanelFile(t *testing.T, m *Model) {
	t.Helper()
	const path = "pkg/tui/panel.go"
	for i, r := range m.currentRows() {
		if r.kind == rowFile && r.path == path {
			m.cursor = i
			m.rememberCursor()
			return
		}
	}
	t.Fatalf("file %s not listed", path)
}

func TestNavigatorShowsRepositoryWorktreeAndGroupIdentity(t *testing.T) {
	m := New("ws")
	m.SetSize(120, 24)
	drain(m.SetSnapshot(sampleSnapshot()))
	m.Focus()
	view := plain(m)
	t.Log("\n" + view)
	for _, want := range []string{"Changes  Draft 0", "docker-agent", "● ~/dev/ai-dev/docker-agent", "○ ./wt/shell", "⎇ shell-wt · linked", "Conflicts 1", "Staged 1", "Unstaged 1", "Untracked 1", "⊘ +2 linked outside · not shown", "kit/vendor/foo  ⎇ detached @3e336a8"} {
		assert.Contains(t, view, want)
	}
	assert.NotContains(t, view, "pkg/tui/panel.go", "groups start collapsed")
	assert.NotContains(t, view, "as of", "no status clutter")
	r, ok := m.selectedRow()
	require.True(t, ok)
	assert.Equal(t, worktreeKey("w1"), r.key, "selection reveals the session's own checkout")
	assertFrame(t, m.View(), 120, 24)
}

func TestOnlyTheCurrentCheckoutPathIsRevealed(t *testing.T) {
	m := New("ws")
	m.SetSize(120, 24)
	drain(m.SetSnapshot(sampleSnapshot()))
	expanded := map[string]bool{}
	for _, r := range m.currentRows() {
		if r.structural() {
			expanded[r.key] = r.expanded
		}
	}
	assert.True(t, expanded[repoKey("r1")])
	assert.True(t, expanded[worktreeKey("w1")])
	assert.False(t, expanded[worktreeKey("w2")])
	assert.False(t, expanded[worktreeKey("w3")], "nested repositories stay collapsed")
	assert.False(t, expanded[groupKey("w1", GroupUnstaged)], "groups never auto-expand")
}

func TestFramesStayExactAcrossSizes(t *testing.T) {
	for _, size := range [][2]int{{160, 40}, {120, 20}, {99, 20}, {60, 12}, {30, 6}, {20, 3}, {200, 5}} {
		m := newModel(t, size[0], size[1])
		assertFrame(t, m.View(), size[0], size[1])
		selectPanelFile(t, m)
		msgs := keys(t, m, "enter")
		req, ok := find[OpenDocumentRequestMsg](msgs)
		require.True(t, ok)
		m.SetDocument(sampleDocument(req.Key))
		assertFrame(t, m.View(), size[0], size[1])
	}
}

func TestKeyboardOpensFileAndReadsHunks(t *testing.T) {
	m := newModel(t, 120, 24)
	selectPanelFile(t, m)
	msgs := keys(t, m, "enter")
	req, ok := find[OpenDocumentRequestMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, DocumentKey{Mode: ModeChanges, WorktreeID: "w1", Path: "pkg/tui/panel.go", Group: GroupUnstaged}, req.Key)
	_, ok = find[SelectionChangedMsg](msgs)
	assert.True(t, ok)

	m.SetDocument(sampleDocument(req.Key))
	view := plain(m)
	t.Log("\n" + view)
	assert.Contains(t, view, "pkg/tui/panel.go")
	assert.Contains(t, view, "Unstaged · hunk 1/2 · +2 −1")
	assert.Contains(t, view, "+ label, hint")

	keys(t, m, "]")
	assert.Equal(t, 4, m.reader.line)
	assert.Contains(t, plain(m), "hunk 2/2")
	keys(t, m, "[")
	assert.Equal(t, 0, m.reader.line)
	keys(t, m, "esc")
	assert.Equal(t, areaNavigator, m.area)
}

func TestStaleDocumentIsIgnored(t *testing.T) {
	m := newModel(t, 120, 24)
	selectPanelFile(t, m)
	keys(t, m, "enter")
	m.SetDocument(sampleDocument(DocumentKey{Mode: ModeChanges, WorktreeID: "w1", Path: "other.go", Group: GroupUnstaged}))
	assert.Equal(t, StateLoading, m.doc.State)
}

func TestSearchUsesMatchKeysWithoutStealingHunkKeys(t *testing.T) {
	m := newModel(t, 120, 24)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))

	keys(t, m, "/")
	require.True(t, m.Capturing())
	handled, _ := m.HandleKey(press("ctrl+n"))
	assert.True(t, handled, "tab bar keys are swallowed while typing")
	keys(t, m, "l", "a", "b", "e", "l", "enter")
	assert.False(t, m.Capturing())
	assert.Equal(t, []int{2, 3, 5, 6}, m.reader.matches)
	assert.Equal(t, 2, m.reader.line)
	keys(t, m, "n")
	assert.Equal(t, 3, m.reader.line)
	keys(t, m, "N")
	assert.Equal(t, 2, m.reader.line)
	assert.Contains(t, plain(m), "/label 1/4")
	keys(t, m, "]")
	assert.Equal(t, 4, m.reader.line, "] still moves by hunk with a search active")

	keys(t, m, "esc")
	assert.Empty(t, m.search.Value(), "first Esc clears the search")
	assert.Equal(t, areaReader, m.area)
}

func TestTypingInFilterNeverNavigates(t *testing.T) {
	m := newModel(t, 120, 24)
	before := m.cursor
	keys(t, m, "/", "j", "k")
	assert.Equal(t, "jk", m.filter.Value())
	assert.True(t, m.Capturing())
	keys(t, m, "esc")
	assert.False(t, m.Capturing())
	assert.Empty(t, m.filter.Value())
	assert.Equal(t, before, m.cursor)

	keys(t, m, "/", "p", "a", "n", "e", "l", "enter")
	view := plain(m)
	assert.Contains(t, view, "pkg/tui/panel.go")
	assert.NotContains(t, view, "tabs.go")
	r, ok := m.selectedRow()
	require.True(t, ok)
	assert.Equal(t, "pkg/tui/panel.go", r.path, "filter lands on the first match")
	keys(t, m, "esc")
	assert.Contains(t, plain(m), "tabs.go")
}

func TestCopyTextKeepsMarkersAndPatchCopiesWholeHunks(t *testing.T) {
	m := newModel(t, 120, 24)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	m.reader.line = 2

	copied, ok := find[CopyRequestMsg](keys(t, m, "v", "j", "x"))
	require.True(t, ok)
	assert.Equal(t, CopyText, copied.Kind)
	assert.Equal(t, "-label := \"\"\n+label, hint := \"\", \"\"\n", copied.Text)
	assert.Equal(t, "Copied 2 lines (text)", copied.Summary)

	patch, ok := find[CopyRequestMsg](keys(t, m, "C"))
	require.True(t, ok)
	assert.Equal(t, CopyPatch, patch.Kind)
	assert.True(t, strings.HasPrefix(patch.Text, "diff --git"))
	assert.Contains(t, patch.Text, "@@ -1,3 +1,3 @@ func a()\n package tui\n-label := \"\"\n+label, hint := \"\", \"\"\n")
	assert.NotContains(t, patch.Text, "func b()")

	path, ok := find[CopyRequestMsg](keys(t, m, "y"))
	require.True(t, ok)
	assert.Equal(t, CopyPath, path.Kind)
	assert.Equal(t, "pkg/tui/panel.go", path.Text)
}

func TestPatchCopyDisabledForTruncatedBinaryAndConflicts(t *testing.T) {
	for name, mutate := range map[string]func(*Document){
		"truncated": func(d *Document) { d.Truncated = true },
		"binary":    func(d *Document) { d.Binary = true },
		"conflict":  func(d *Document) { d.Key.Group = GroupConflicts },
		"no header": func(d *Document) { d.PatchHeader = "" },
	} {
		t.Run(name, func(t *testing.T) {
			m := newModel(t, 120, 24)
			selectPanelFile(t, m)
			req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
			doc := sampleDocument(req.Key)
			mutate(&doc)
			m.requested = doc.Key
			m.SetDocument(doc)
			m.area = areaReader
			_, ok := find[CopyRequestMsg](keys(t, m, "C"))
			assert.False(t, ok)
		})
	}
}

func TestHiddenWorktreesDiscloseNothing(t *testing.T) {
	m := newModel(t, 120, 24)
	for i, r := range m.currentRows() {
		if r.kind == rowHidden {
			m.cursor = i
		}
	}
	msgs := keys(t, m, "y", "enter")
	_, copied := find[CopyRequestMsg](msgs)
	assert.False(t, copied)
	assert.Contains(t, plain(m), "Outside · hidden")
}

func TestExpandingUnscannedWorktreeRequestsScanOnce(t *testing.T) {
	m := newModel(t, 120, 24)
	m.expanded[worktreeKey("w2")] = false
	m.invalidateRows()
	for i, r := range m.currentRows() {
		if r.key == worktreeKey("w2") {
			m.cursor = i
		}
	}
	req, ok := find[LoadWorktreeRequestMsg](keys(t, m, "right"))
	require.True(t, ok)
	assert.Equal(t, "w2", req.WorktreeID)
	assert.Contains(t, plain(m), "scanning…")
	keys(t, m, "left")
	_, again := find[LoadWorktreeRequestMsg](keys(t, m, "right"))
	assert.False(t, again)
}

func TestFilesModeWorksWithoutGit(t *testing.T) {
	m := New("ws")
	m.SetSize(120, 20)
	drain(m.SetSnapshot(Snapshot{Generation: 1, Scope: Scope{Label: "~/notes"}, Repositories: []Repository{
		{ID: "", Name: "notes", Kind: RepoNone, Worktrees: []Worktree{{ID: "ws-root", Path: "~/notes", Current: true, State: StateReady}}},
	}}))
	m.Focus()
	msgs := drain(m.SetMode(ModeFiles))
	req, ok := find[LoadDirectoryRequestMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, LoadDirectoryRequestMsg{Owner: "ws", WorktreeID: "ws-root", Path: ""}, req)

	assert.Nil(t, drain(m.SetDirectory(Directory{WorktreeID: "ws-root", State: StateReady, Entries: []Entry{{Name: "b.md"}, {Name: "docs", Dir: true}, {Name: "a.md"}}})))
	view := plain(m)
	t.Log("\n" + view)
	assert.NotContains(t, view, "⎇")
	assert.Less(t, strings.Index(view, "docs/"), strings.Index(view, "a.md"), "folders first")

	keys(t, m, "down")
	dir, ok := find[LoadDirectoryRequestMsg](keys(t, m, "enter"))
	require.True(t, ok)
	assert.Equal(t, "docs", dir.Path)
	drain(m.SetDirectory(Directory{WorktreeID: "ws-root", Path: "docs", State: StateReady, Entries: []Entry{{Name: "guide.md"}}}))

	selectEntry := func(p string) {
		for i, r := range m.currentRows() {
			if r.kind == rowEntry && r.path == p {
				m.cursor = i
				m.rememberCursor()
			}
		}
	}
	selectEntry("docs/guide.md")
	open, ok := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	require.True(t, ok)
	assert.Equal(t, DocumentKey{Mode: ModeFiles, WorktreeID: "ws-root", Path: "docs/guide.md"}, open.Key)
	m.SetDocument(Document{Key: open.Key, State: StateReady, Lines: []Line{{New: 1, Text: "# Guide"}}})
	copied, _ := find[CopyRequestMsg](keys(t, m, "x"))
	assert.Equal(t, "# Guide\n", copied.Text, "file text copies without diff markers")
}

func TestNavigatorUsesCheckoutPathAsPrimaryWorktreeLabel(t *testing.T) {
	snap := sampleSnapshot()
	snap.Repositories[0].Worktrees[0].Path = "."
	snap.Repositories[0].Worktrees[1].Path = "worktrees/feature"
	snap.Repositories[0].Worktrees[1].Branch = "feature-branch"
	snap.Repositories[0].HiddenOutside = 0
	snap.Repositories[1].Worktrees[0].Path = "nested/checkout"
	snap.Repositories[1].Worktrees[0].Detached = "3e336a8"
	m := New("ws")
	m.SetSize(120, 24)
	drain(m.SetSnapshot(snap))
	m.Focus()
	m.expanded[repoKey("r1")] = true
	m.expanded[repoKey("r2")] = true
	m.invalidateRows()
	view := plain(m)
	assert.Contains(t, view, "● docker-agent  ⎇ main", "merged dot path falls back to repository name")
	assert.Contains(t, view, "○ worktrees/feature", "linked checkout path is primary")
	assert.Contains(t, view, "⎇ feature-br…", "narrow row truncates branch metadata separately")
	assert.Contains(t, view, "nested/checkout  ⎇ detached @3e336a…", "merged detached checkout path is primary")
}

func TestWorktreeRowsShowCheckoutPathBeforeBranchMetadata(t *testing.T) {
	m := New("ws")
	merged := sampleSnapshot().Repositories[1]
	wt := merged.Worktrees[0]
	wt.Path = "nested/checkout"
	mergedRow := row{merged: true}
	text := ansi.Strip(m.worktreeCells(mergedRow, merged, wt, styles.BaseStyle))
	assert.Contains(t, text, "nested/checkout", "checkout is primary")
	assert.Contains(t, text, "detached @3e336a8", "detached HEAD is secondary")
	assert.Less(t, strings.Index(text, "nested/checkout"), strings.Index(text, "detached @"))

	repo := sampleSnapshot().Repositories[0]
	root := repo.Worktrees[0]
	root.Path = "."
	rootText := ansi.Strip(m.worktreeCells(mergedRow, repo, root, styles.BaseStyle))
	assert.Contains(t, rootText, "docker-agent", "dot checkout uses repository name")
	assert.Contains(t, rootText, "⎇ main")

	child := repo.Worktrees[1]
	child.Path = "worktrees/shell"
	childRow := row{merged: false}
	childText := ansi.Strip(m.worktreeCells(childRow, repo, child, styles.BaseStyle))
	assert.Contains(t, childText, "worktrees/shell")
	assert.Contains(t, childText, "⎇ shell-wt")
	assert.Less(t, strings.Index(childText, "worktrees/shell"), strings.Index(childText, "shell-wt"))
}

func TestNarrowDrilldownAndEscapeChain(t *testing.T) {
	m := newModel(t, 80, 20)
	assert.NotContains(t, plain(m), "Reader")
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	assert.True(t, m.drilled)
	assert.Contains(t, plain(m), "Unstaged · hunk 1/2")
	assert.NotContains(t, plain(m), "Changes  Draft 0")

	keys(t, m, "esc")
	assert.False(t, m.drilled)
	assert.Contains(t, plain(m), "Changes  Draft 0")
	_, closed := find[CloseRequestMsg](keys(t, m, "esc"))
	assert.True(t, closed)
}

func TestShiftCRemainsPatchCopyShortcut(t *testing.T) {
	m := openThreeHunks(t, 140, 12, false)
	msg := tea.KeyPressMsg{Code: 'c', ShiftedCode: 'C', Mod: tea.ModShift, Text: "C"}
	_, cmd := m.HandleKey(msg)
	patch, ok := find[CopyRequestMsg](drain(cmd))
	require.True(t, ok)
	assert.Equal(t, CopyPatch, patch.Kind)
}

func TestHostKeysPassThroughAndDestructiveKeysAreInert(t *testing.T) {
	m := newModel(t, 120, 24)
	for _, k := range []string{"tab", "ctrl+c", "ctrl+k", "ctrl+n", "ctrl+p", "ctrl+t"} {
		handled, _ := m.HandleKey(press(k))
		assert.False(t, handled, k)
	}
	handled, cmd := m.HandleKey(press("ctrl+w"))
	assert.True(t, handled)
	_, closed := find[CloseRequestMsg](drain(cmd))
	assert.False(t, closed)
	for _, k := range []string{"s", "d", "u", "x"} {
		handled, cmd := m.HandleKey(press(k))
		assert.True(t, handled, "printable %s never reaches the composer", k)
		assert.Empty(t, drain(cmd))
	}
}

func TestMouseOwnsRectangleAndScrolls(t *testing.T) {
	m := newModel(t, 120, 24)
	m.SetPosition(2, 1)
	handled, _ := m.HandleMouse(tea.MouseMotionMsg{X: 0, Y: 0})
	assert.False(t, handled, "outside the rectangle")
	for _, msg := range []tea.MouseMsg{tea.MouseMotionMsg{X: 50, Y: 10}, tea.MouseReleaseMsg{X: 50, Y: 10}, tea.MouseClickMsg{X: 50, Y: 10, Button: tea.MouseRight}} {
		handled, _ := m.HandleMouse(msg)
		assert.True(t, handled)
	}

	g := m.geometry()
	var fileRow int
	for i, r := range m.currentRows() {
		if r.path == "pkg/tui/panel.go" {
			fileRow = i
		}
	}
	m.Blur()
	_, cmd := m.HandleMouse(tea.MouseClickMsg{X: 2 + g.navX + 5, Y: 1 + g.navBodyY + fileRow - m.navTop, Button: tea.MouseLeft})
	msgs := drain(cmd)
	_, focus := find[FocusRequestMsg](msgs)
	assert.True(t, focus)
	req, ok := find[OpenDocumentRequestMsg](msgs)
	require.True(t, ok)
	assert.Equal(t, "pkg/tui/panel.go", req.Key.Path)

	doc := sampleDocument(req.Key)
	for i := range 200 {
		doc.Lines = append(doc.Lines, Line{Kind: LineContext, Old: 100 + i, New: 101 + i, Text: "filler"})
	}
	m.SetDocument(doc)
	m.HandleMouse(tea.MouseWheelMsg{X: 2 + g.readerX + 3, Y: 5, Button: tea.MouseWheelDown})
	assert.Equal(t, 2, m.reader.top)

	m.Focus()
	_, cmd = m.HandleMouse(tea.MouseClickMsg{X: 2 + g.navX + 1, Y: 1, Button: tea.MouseLeft})
	assert.Nil(t, drain(cmd))
	m.HandleMouse(tea.MouseClickMsg{X: 2 + g.navX + 12, Y: 1, Button: tea.MouseLeft})
	assert.True(t, m.draftOpen, "the Draft tab is clickable")
	assert.Equal(t, ModeChanges, m.mode, "surfaces never switch mode")
}

func TestTargetMismatchAndSaveRestore(t *testing.T) {
	m := newModel(t, 120, 24)
	_, mismatch := m.TargetMismatch()
	assert.False(t, mismatch)
	for i, r := range m.currentRows() {
		if r.key == worktreeKey("w3") {
			m.cursor = i
		}
	}
	label, mismatch := m.TargetMismatch()
	assert.True(t, mismatch)
	assert.Contains(t, label, "detached @3e336a8")

	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(sampleDocument(req.Key))
	keys(t, m, "j", "j", "/", "h", "i", "n", "t", "enter")
	saved := m.Save()

	restored := newModel(t, 120, 24)
	reopen, ok := find[OpenDocumentRequestMsg](drain(restored.Restore(saved)))
	require.True(t, ok)
	restored.SetDocument(sampleDocument(reopen.Key))
	assert.Equal(t, m.reader.line, restored.reader.line)
	assert.Equal(t, areaReader, restored.area)
	assert.Equal(t, "hint", restored.search.Value())
	assert.Equal(t, m.lastKey, restored.lastKey)
}

func TestRemoteScopeStaysLabelledAndUnavailable(t *testing.T) {
	m := New("ws")
	m.SetSize(100, 12)
	drain(m.SetSnapshot(Snapshot{Generation: 1, Scope: Scope{Label: "kit:/workspace/docker-agent", Remote: true, RemoteLabel: "Kit hackerspace", Unavailable: "Git review reads local repositories only. Remote changes aren't available yet."}}))
	view := plain(m)
	t.Log("\n" + view)
	assert.Contains(t, view, "remote")
	assert.Contains(t, view, "Git review reads local")
	assert.Contains(t, view, "Remote changes aren't available")
	assert.NotContains(t, view, "Select a file")
}

func TestLargeDocumentRendersOnlyViewport(t *testing.T) {
	m := newModel(t, 140, 30)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	doc := Document{Key: req.Key, State: StateReady}
	for i := range 200000 {
		doc.Lines = append(doc.Lines, Line{Kind: LineAdd, New: i + 1, Text: strings.Repeat("x", 300)})
	}
	m.SetDocument(doc)
	keys(t, m, "G")
	assert.Equal(t, 199999, m.reader.line)
	assertFrame(t, m.View(), 140, 30)
}

func TestFilesFilterStatesLoadedFolderScope(t *testing.T) {
	m := newModel(t, 120, 20)
	assert.Contains(t, m.filter.Placeholder, "changed files")
	drain(m.SetMode(ModeFiles))
	assert.Equal(t, "filter loaded folders only…", m.filter.Placeholder)
	keys(t, m, "/", "z", "q")
	view := plain(m)
	assert.Contains(t, view, "No matches for “zq” in loaded folders")
	keys(t, m, "enter")
	assert.Contains(t, plain(m), "/ zq · loaded folders only")
	_, sections := m.Help()
	var filterHelp string
	for _, e := range sections[0].Entries {
		if e.ID == "review.nav.filter" {
			filterHelp = e.Description
		}
	}
	assert.Contains(t, filterHelp, "only already loaded folders in Files")
}

func longDocument(key DocumentKey, n int) Document {
	doc := Document{Key: key, State: StateReady}
	for i := range n {
		doc.Lines = append(doc.Lines, Line{Kind: LineContext, Old: i + 1, New: i + 1, Text: "line " + strconv.Itoa(i)})
	}
	return doc
}

// scrollbarRows reports, for each body row of a drilled single-column reader,
// whether the last cell is the scrollbar thumb, track, or blank.
func scrollbarRows(t *testing.T, m *Model) []string {
	t.Helper()
	thumb, track := styles.ThumbStyle.Render("│"), styles.TrackStyle.Render("│")
	var out []string
	for _, line := range strings.Split(m.View(), "\n")[m.readerHeaderRows():] {
		switch {
		case strings.HasSuffix(line, thumb):
			out = append(out, "thumb")
		case strings.HasSuffix(line, track):
			out = append(out, "track")
		default:
			out = append(out, "blank")
		}
	}
	return out
}

func TestReaderHeaderHasSeparateBlankRowBeforeDiff(t *testing.T) {
	m := openThreeHunks(t, 80, 12, false)
	lines := strings.Split(m.View(), "\n")
	require.GreaterOrEqual(t, len(lines), 3)
	assert.NotEmpty(t, ansi.Strip(lines[0]), "file name and status stay in the title")
	assert.Empty(t, strings.TrimSpace(ansi.Strip(lines[1])), "one blank row separates metadata from diff content")
	assert.Contains(t, ansi.Strip(lines[2]), "@@", "diff starts below the separator")
}

func TestReaderUsesSharedScrollbarAndFade(t *testing.T) {
	m := newModel(t, 70, 12)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	m.SetDocument(longDocument(req.Key, 200))
	require.True(t, m.drilled)

	top := scrollbarRows(t, m)
	assert.Equal(t, "thumb", top[0], "thumb starts at the top")
	assert.NotContains(t, top, "blank", "scrollbar spans the body")
	firstRow := func() string { return strings.Split(m.View(), "\n")[m.readerHeaderRows()] }
	plainFirst := m.reader.render(m.readerWidth(), m.readerBodyHeight(), true, matcher{})[0]
	assert.True(t, strings.HasPrefix(firstRow(), plainFirst), "no top fade at the start")

	keys(t, m, "pgdown", "pgdown", "pgdown")
	middle := scrollbarRows(t, m)
	assert.Equal(t, "track", middle[0])
	assert.Equal(t, "track", middle[len(middle)-1])
	assert.Contains(t, middle, "thumb")
	plainFirst = m.reader.render(m.readerWidth(), m.readerBodyHeight(), true, matcher{})[0]
	assert.False(t, strings.HasPrefix(firstRow(), plainFirst), "content above fades at the top edge")

	keys(t, m, "G")
	end := scrollbarRows(t, m)
	assert.Equal(t, "thumb", end[len(end)-1], "thumb reaches the bottom")
	assert.Equal(t, m.readSV.MaxScrollOffset(), m.reader.top)

	m.SetSize(70, 30)
	short := longDocument(req.Key, 3)
	m.SetDocument(short)
	assert.NotContains(t, scrollbarRows(t, m), "thumb", "no scrollbar when the content fits")
	assertFrame(t, m.View(), 70, 30)
}

func TestReaderScrollbarCountsWrappedRows(t *testing.T) {
	m := newModel(t, 60, 10)
	selectPanelFile(t, m)
	req, _ := find[OpenDocumentRequestMsg](keys(t, m, "enter"))
	doc := Document{Key: req.Key, State: StateReady}
	for i := range 4 {
		doc.Lines = append(doc.Lines, Line{Kind: LineAdd, New: i + 1, Text: strings.Repeat("wrap ", 40)})
	}
	m.SetDocument(doc)
	m.View()
	assert.Greater(t, m.reader.total, 4, "wrapped lines add visual rows")
	assert.Equal(t, m.reader.total-m.readerBodyHeight(), m.readSV.MaxScrollOffset(), "scroll range follows wrapped rows")
	assert.Contains(t, scrollbarRows(t, m), "thumb")
}

func TestNavigatorScrollbarAndWheel(t *testing.T) {
	snap := sampleSnapshot()
	files := snap.Repositories[0].Worktrees[0].Files
	for i := range 60 {
		files = append(files, FileChange{Path: "gen/file" + strconv.Itoa(i) + ".go", Group: GroupUnstaged, Status: "M"})
	}
	snap.Repositories[0].Worktrees[0].Files = files
	m := New("ws")
	m.SetSize(80, 12)
	drain(m.SetSnapshot(snap))
	m.Focus()
	expandAll(m)
	thumb := styles.ThumbStyle.Render("│")
	rows := strings.Split(m.View(), "\n")
	assert.True(t, strings.HasSuffix(rows[1], thumb), "long lists show the shared scrollbar")

	m.HandleMouse(tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelDown})
	assert.Equal(t, 2, m.navTop, "wheel scrolls the navigator by two rows per tick")
	for range 40 {
		m.HandleMouse(tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelDown})
	}
	rows = strings.Split(m.View(), "\n")
	assert.True(t, strings.HasSuffix(rows[len(rows)-1], thumb), "thumb at the end")
	assert.Equal(t, m.navSV.MaxScrollOffset(), m.navTop)
	keys(t, m, "g")
	assert.Zero(t, m.navTop, "keyboard jumps keep the cursor visible")
}

func TestPartialDiscoveryNoteIsQuietAndKeepsData(t *testing.T) {
	snap := sampleSnapshot()
	snap.Scope.Note = "Discovery limited; some repositories were not scanned"
	m := New("ws")
	m.SetSize(120, 24)
	drain(m.SetSnapshot(snap))
	view := plain(m)
	assert.Contains(t, view, "Discovery limited; some")
	assert.Contains(t, view, "● ~/dev/ai-dev/docker-agent", "found checkouts stay visible")
	assert.NotContains(t, view, "may be outdated", "a partial result is not stale")

	drain(m.SetSnapshot(sampleSnapshot()))
	assert.NotContains(t, plain(m), "Discovery limited", "no note for complete results")
}
