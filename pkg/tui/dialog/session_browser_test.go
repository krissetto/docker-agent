package dialog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionBrowserNavigation(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)

	// Initialize and set window size like the TUI does
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially selected should be 0
	require.Equal(t, 0, d.selected, "initial selection should be 0")

	// Test that key bindings match correctly
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	t.Logf("Down key matches keyMap.Down: %v", key.Matches(downKey, d.keyMap.Down))
	t.Logf("Up key matches keyMap.Up: %v", key.Matches(upKey, d.keyMap.Up))

	require.True(t, key.Matches(downKey, d.keyMap.Down), "down key should match keyMap.Down")
	require.True(t, key.Matches(upKey, d.keyMap.Up), "up key should match keyMap.Up")

	// Press down arrow
	updated, _ := d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after down arrow")

	// Press down again
	updated, _ = d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.selected, "selection should be 2 after second down arrow")

	// Press down again (should stay at 2 since we're at the end)
	updated, _ = d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.selected, "selection should stay at 2 at end of list")

	// Press up arrow
	updated, _ = d.Update(upKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after up arrow")
}

func TestSessionBrowserNavigationWithCtrl(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Test ctrl+j for down
	ctrlJ := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	t.Logf("ctrl+j matches keyMap.Down: %v", key.Matches(ctrlJ, d.keyMap.Down))

	updated, _ := d.Update(ctrlJ)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after ctrl+j")

	// Test ctrl+k for up
	ctrlK := tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}
	t.Logf("ctrl+k matches keyMap.Up: %v", key.Matches(ctrlK, d.keyMap.Up))

	updated, _ = d.Update(ctrlK)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.selected, "selection should be 0 after ctrl+k")

	// Verify ctrl+y is bound to CopyID and doesn't collide with Up.
	// We only assert key matching here to avoid clipboard side-effects in tests.
	ctrlY := tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
	require.True(t, key.Matches(ctrlY, d.keyMap.CopyID), "ctrl+y should match keyMap.CopyID")
	require.False(t, key.Matches(ctrlY, d.keyMap.Up), "ctrl+y should not match keyMap.Up")
}

func TestSessionBrowserViewShowsSelection(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initial view should show first session selected
	view1 := d.View()
	t.Logf("Initial view (selection=0):\n%s", view1)

	// Navigate down
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	d.Update(downKey)

	// View should now show second session selected
	view2 := d.View()
	t.Logf("After down (selection=1):\n%s", view2)

	// The views should be different
	require.NotEqual(t, view1, view2, "view should change after navigation")
}

func TestSessionBrowserFiltersEmptySessions(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
		{ID: "4", Title: "", CreatedAt: time.Now()},
		{ID: "5", Title: "Session 5", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)

	// Should only have non-empty sessions
	require.Len(t, d.sessions, 3, "should have 3 non-empty sessions")
	require.Len(t, d.filtered, 3, "filtered should also have 3 sessions")

	// Verify the correct sessions are kept
	require.Equal(t, "1", d.sessions[0].ID)
	require.Equal(t, "3", d.sessions[1].ID)
	require.Equal(t, "5", d.sessions[2].ID)
}

func TestSessionBrowserAllEmptySessions(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "", CreatedAt: time.Now()},
		{ID: "2", Title: "", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)

	// Should have no sessions
	require.Empty(t, d.sessions, "should have 0 sessions")
	require.Empty(t, d.filtered, "filtered should also have 0 sessions")
}

func TestSessionBrowserScrolling(t *testing.T) {
	t.Parallel()
	// Create more sessions than can fit in view
	sessions := make([]session.Summary, 20)
	for i := range sessions {
		sessions[i] = session.Summary{
			ID:        strconv.Itoa(i + 1),
			Title:     fmt.Sprintf("Session %d", i+1),
			CreatedAt: time.Now(),
		}
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	// Set a small window size to force scrolling
	d.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	maxVisible := d.scrollview.VisibleHeight()
	t.Logf("Max visible items: %d", maxVisible)

	// Navigate down past the visible area
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	for range maxVisible + 2 {
		d.Update(downKey)
	}

	// Selected should be beyond initial visible area
	require.Greater(t, d.selected, maxVisible-1, "selected should be beyond initial visible area")

	// Call View() to trigger offset adjustment (like the TUI does)
	view := d.View()

	scrollOffset := d.scrollview.ScrollOffset()
	t.Logf("Selected: %d, ScrollOffset: %d", d.selected, scrollOffset)

	// The scroll offset should have adjusted so selected is visible
	require.LessOrEqual(t, scrollOffset, d.selected, "scroll offset should be <= selected")
	require.Less(t, d.selected, scrollOffset+maxVisible, "selected should be within visible range")

	// Verify the view shows the selected session
	expectedTitle := fmt.Sprintf("Session %d", d.selected+1)
	require.Contains(t, view, expectedTitle, "view should contain selected session")
}

func TestSessionBrowserMouseClickSelectsSession(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially selected should be 0
	require.Equal(t, 0, d.selected)

	// Get the dialog position to calculate where to click
	dialogRow, _ := d.Position()
	listStartY := dialogRow + sessionBrowserListStartY

	// Single-click on the second session (line index 1)
	clickMsg := tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 1,
		Button: tea.MouseLeft,
	}
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	// Selection should have moved to session 2
	require.Equal(t, 1, d.selected, "single click should select session")
	// Single click should not produce a load command
	require.Nil(t, cmd, "single click should not trigger load")

	// Single-click on the third session
	clickMsg = tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 2,
		Button: tea.MouseLeft,
	}
	updated, cmd = d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	require.Equal(t, 2, d.selected, "single click should select third session")
	require.Nil(t, cmd, "single click on different session should not trigger load")
}

func TestSessionBrowserDoubleClickOpensSession(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "sess-1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "sess-2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "sess-3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	dialogRow, _ := d.Position()
	listStartY := dialogRow + sessionBrowserListStartY

	// First click selects
	clickMsg := tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 1,
		Button: tea.MouseLeft,
	}
	updated, _ := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected)

	// Second click on the same item (double-click) should trigger load
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should stay on double-clicked session")
	require.NotNil(t, cmd, "double-click should produce a command to load the session")
}

func TestSessionBrowserClickOutsideListIgnored(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Click way outside the list area
	clickMsg := tea.MouseClickMsg{
		X:      5,
		Y:      0,
		Button: tea.MouseLeft,
	}
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	// Selection should remain at 0
	require.Equal(t, 0, d.selected, "click outside list should not change selection")
	require.Nil(t, cmd, "click outside list should not produce a command")
}

func workspaceTestSessions() []session.Summary {
	project := filepath.Join(string(filepath.Separator), "work", "project")
	other := filepath.Join(string(filepath.Separator), "work", "other")
	return []session.Summary{
		{ID: "here-1", Title: "Newest here", CreatedAt: time.Now(), WorkingDir: project},
		{ID: "away-1", Title: "Away one", CreatedAt: time.Now().Add(-1 * time.Hour), WorkingDir: other},
		{ID: "here-2", Title: "Older here", CreatedAt: time.Now().Add(-2 * time.Hour), WorkingDir: project},
		{ID: "nodir", Title: "No dir recorded", CreatedAt: time.Now().Add(-3 * time.Hour)},
	}
}

func filteredIDs(d *sessionBrowserDialog) []string {
	ids := make([]string, 0, len(d.filtered))
	for _, s := range d.filtered {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestSessionBrowserWorkspaceGrouping(t *testing.T) {
	t.Parallel()
	project := filepath.Join(string(filepath.Separator), "work", "project")
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), project)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Workspace sessions come first (keeping recency order), then the rest.
	require.Equal(t, []string{"here-1", "here-2", "away-1", "nodir"}, filteredIDs(d))
	require.Equal(t, 2, d.workspaceCount)
	require.Equal(t, 2, d.elsewhereCount)

	// Rows carry two section headers around the groups.
	require.Len(t, d.rows, 6, "4 sessions + 2 headers")
	require.Equal(t, sessionBrowserHeaderWorkspace, d.rows[0].header)
	require.Equal(t, -1, d.rows[0].sessionIdx)
	require.Equal(t, sessionBrowserHeaderElsewhere, d.rows[3].header)
	require.Equal(t, -1, d.rows[3].sessionIdx)

	// rowForSession maps every session to its visual row.
	require.Equal(t, []int{1, 2, 4, 5}, d.rowForSession)

	view := d.View()
	require.Contains(t, view, sessionBrowserHeaderWorkspace)
	require.Contains(t, view, sessionBrowserHeaderElsewhere)
	require.Contains(t, view, filepath.Join(string(filepath.Separator), "work", "other"), "sessions from another workspace should show their directory")
}

func TestSessionBrowserWorkspaceGroupingFlatWithoutWorkspace(t *testing.T) {
	t.Parallel()
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), "")
	d := dialog.(*sessionBrowserDialog)

	// Unknown workspace: original order, no headers.
	require.Equal(t, []string{"here-1", "away-1", "here-2", "nodir"}, filteredIDs(d))
	require.Len(t, d.rows, 4)
	for i, row := range d.rows {
		require.Empty(t, row.header)
		require.Equal(t, i, row.sessionIdx)
	}
}

func TestSessionBrowserNoHeadersWhenSingleGroup(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now(), WorkingDir: "/work/project"},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now(), WorkingDir: "/work/project"},
	}

	dialog := NewSessionBrowserDialog(sessions, "/work/project")
	d := dialog.(*sessionBrowserDialog)

	require.Len(t, d.rows, 2, "headers should not appear when every session is in the workspace")
	for _, row := range d.rows {
		require.Empty(t, row.header)
	}
}

func TestSessionBrowserWorkspaceFilterCycle(t *testing.T) {
	t.Parallel()
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), filepath.Join(string(filepath.Separator), "work", "project"))
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	require.True(t, key.Matches(ctrlG, d.keyMap.FilterWorkspace), "ctrl+g should match keyMap.FilterWorkspace")

	// 1: this workspace only
	updated, _ := d.Update(ctrlG)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.workspaceFilter)
	require.Equal(t, []string{"here-1", "here-2"}, filteredIDs(d))
	require.Len(t, d.rows, 2, "no headers in filtered views")

	// 2: other locations only, including sessions without a recorded dir
	updated, _ = d.Update(ctrlG)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.workspaceFilter)
	require.Equal(t, []string{"away-1", "nodir"}, filteredIDs(d))

	// 0: back to all
	updated, _ = d.Update(ctrlG)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.workspaceFilter)
	require.Len(t, d.filtered, 4)
}

func TestSessionBrowserWorkspaceFilterNoopWithoutWorkspace(t *testing.T) {
	t.Parallel()
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	updated, _ := d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.workspaceFilter, "workspace filter should be inert when the workspace is unknown")
	require.Len(t, d.filtered, 4)
}

func TestSessionBrowserWorkspaceFilterCombinesWithStarAndSearch(t *testing.T) {
	t.Parallel()
	sessions := workspaceTestSessions()
	sessions[0].Starred = true // here-1
	sessions[1].Starred = true // away-1

	dialog := NewSessionBrowserDialog(sessions, "/work/project")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Workspace-only + starred-only
	d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	updated, _ := d.Update(tea.KeyPressMsg{Code: 'f', Mod: tea.ModCtrl})
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, []string{"here-1"}, filteredIDs(d))

	// Adding a non-matching search empties the list without panicking.
	d.textInput.SetValue("zzz")
	d.filterSessions()
	require.Empty(t, d.filtered)
	require.Empty(t, d.rows)
	require.NotEmpty(t, d.View())
}

func TestSessionBrowserSearchByID(t *testing.T) {
	t.Parallel()
	sessions := []session.Summary{
		{ID: "abc-123-def", Title: "First session", CreatedAt: time.Now()},
		{ID: "xyz-789", Title: "Second session", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions, "")
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	tests := []struct {
		query string
		want  []string
	}{
		{"abc-123-def", []string{"abc-123-def"}},        // full ID
		{"abc123def", []string{"abc-123-def"}},          // ID without dashes
		{"c-123-d", []string{"abc-123-def"}},            // partial ID with dashes
		{"c123d", []string{"abc-123-def"}},              // partial ID without dashes
		{"ABC123", []string{"abc-123-def"}},             // case-insensitive
		{"session", []string{"abc-123-def", "xyz-789"}}, // title match still works
		{"nope", []string{}},
	}
	for _, tc := range tests {
		d.textInput.SetValue(tc.query)
		d.filterSessions()
		require.Equal(t, tc.want, filteredIDs(d), "query %q", tc.query)
	}
}

func TestSessionBrowserWorkspacePathNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sessionDir   string
		workspaceDir string
	}{
		{name: "identical", sessionDir: "/work/project", workspaceDir: "/work/project"},
		{name: "trailing slash", sessionDir: "/work/project/", workspaceDir: "/work/project"},
		{name: "redundant segments", sessionDir: "/work/./project/sub/..", workspaceDir: "/work/project"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newWorkspaceMatcher(tc.workspaceDir)
			require.True(t, m.matches(tc.sessionDir))
		})
	}

	require.False(t, newWorkspaceMatcher("/work/project").matches(""), "empty session dir never matches")
	require.False(t, newWorkspaceMatcher("").matches("/work/project"), "unknown workspace never matches")
}

func TestSessionBrowserWorkspaceSymlinkNormalization(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	t.Parallel()

	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(realDir, link))

	m := newWorkspaceMatcher(realDir)
	require.True(t, m.matches(link), "a symlinked session dir should match its resolved workspace")
}

func TestSessionBrowserWorkspaceGitRootNormalization(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	sub := filepath.Join(repo, "pkg", "tui")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	// git worktree layout: wt/.git file -> repo/.git/worktrees/wt (with commondir)
	gitdir := filepath.Join(repo, ".git", "worktrees", "wt")
	require.NoError(t, os.MkdirAll(gitdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644))
	wt := filepath.Join(base, "wt")
	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644))

	m := newWorkspaceMatcher(repo)
	require.True(t, m.matches(sub), "a session in a subdirectory should match its repo root")
	require.True(t, m.matches(wt), "a session in a linked worktree should match the main repo")
	require.False(t, m.matches(base), "a dir outside the repo should not match")

	// Opening the browser from the worktree also groups main-repo sessions.
	require.True(t, newWorkspaceMatcher(wt).matches(repo))

	// The "This workspace" header shows the repository root, not the
	// subdirectory or worktree the browser was opened from.
	require.Equal(t, repo, workspaceDisplayDir(sub))
	require.Equal(t, repo, workspaceDisplayDir(wt))
	require.Equal(t, base, workspaceDisplayDir(base), "non-repo dirs are shown as-is")
}

func TestSessionBrowserWorkspaceDisplayDirThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	t.Parallel()

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	sub := filepath.Join(repo, "pkg")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(sub, link))

	// A symlink straight to the repo root sees .git through the link (Stat
	// follows symlinks) and is shown as-is — it is the repo root, just
	// spelled the way the user reached it.
	linkToRoot := filepath.Join(t.TempDir(), "root-link")
	require.NoError(t, os.Symlink(repo, linkToRoot))
	require.Equal(t, linkToRoot, workspaceDisplayDir(linkToRoot))

	// A symlink to a subdirectory has no .git on its lexical path, so the
	// header falls back to the resolved repository root — matching how the
	// matcher groups these sessions.
	resolved, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, resolved, workspaceDisplayDir(link))
}

func TestSessionBrowserHeaderRowsNotSelectable(t *testing.T) {
	t.Parallel()
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), filepath.Join(string(filepath.Separator), "work", "project"))
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	dialogRow, _ := d.Position()
	listStartY := dialogRow + sessionBrowserListStartY

	// Row 0 is the "This workspace" header: clicking it must not change the
	// selection or produce a command.
	clickMsg := tea.MouseClickMsg{X: 20, Y: listStartY, Button: tea.MouseLeft}
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.selected)
	require.Nil(t, cmd)

	// Row 1 is the first session.
	clickMsg = tea.MouseClickMsg{X: 20, Y: listStartY + 1, Button: tea.MouseLeft}
	updated, _ = d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.selected, "first session row should map to filtered index 0")

	// Row 4 is the first "Other locations" session (after the second header).
	clickMsg = tea.MouseClickMsg{X: 20, Y: listStartY + 4, Button: tea.MouseLeft}
	updated, _ = d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.selected)
}

func TestSessionBrowserNavigationSkipsHeaders(t *testing.T) {
	t.Parallel()
	dialog := NewSessionBrowserDialog(workspaceTestSessions(), filepath.Join(string(filepath.Separator), "work", "project"))
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	for range 3 {
		d.Update(downKey)
	}
	require.Equal(t, 3, d.selected, "selection moves across groups, headers are not selectable stops")

	// Further downs stay clamped at the last session.
	d.Update(downKey)
	require.Equal(t, 3, d.selected)

	upKey := tea.KeyPressMsg{Code: tea.KeyUp}
	for range 5 {
		d.Update(upKey)
	}
	require.Equal(t, 0, d.selected)
}

func TestSessionBrowserPreparedOriginMatchesFirstPositionAndRenderedRows(t *testing.T) {
	d := NewSessionBrowserDialog([]session.Summary{{ID: "one", Title: "First"}, {ID: "two", Title: "Second"}}, "").(*sessionBrowserDialog)
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	bodyX, bodyY, bodyWidth, bodyHeight := d.BodyScrollBounds()
	require.Positive(t, bodyX)
	require.Positive(t, bodyWidth)
	require.Positive(t, bodyHeight)
	row, _ := d.Position()
	require.Equal(t, row+sessionBrowserListStartY, bodyY, "prepared body origin must match the first reported card position")
	view := d.View()
	require.Equal(t, (d.Height()-lipgloss.Height(view))/2, row)
	require.Equal(t, row+sessionBrowserListStartY, bodyY)
}
