package review

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Files lists folders under the current checkout; the global Git discovery
// limit belongs to the Changes repository list and must not leak into Files.
func TestFilesModeDoesNotShowGlobalDiscoveryNoteButChangesDoes(t *testing.T) {
	snap := Snapshot{Generation: 2, Scope: Scope{Label: "~/work", Note: "Partial scan: repository limit reached (100).", LimitReason: "repositories", LimitValue: 100}}
	snap.Repositories = append(snap.Repositories, Repository{ID: "owner", Name: "work", Kind: RepoPrimary, Worktrees: []Worktree{{ID: "w1", Path: ".", Current: true, Branch: "main", State: StateReady}}})
	for i := range 3 {
		id := fmt.Sprintf("sibling-%d", i)
		snap.Repositories = append(snap.Repositories, Repository{ID: id, Name: id, Kind: RepoNested, Worktrees: []Worktree{{ID: id + "-wt", Path: "repos/" + id, State: StateUnscanned}}})
	}
	m := New("ws")
	m.SetSize(120, 30)
	m.SetSession(Session{Title: "main session", WorktreeID: "w1"})
	drain(m.SetSnapshot(snap))
	m.Focus()
	drain(m.SetMode(ModeFiles))
	drain(m.SetDirectory(Directory{WorktreeID: "w1", State: StateReady, Entries: []Entry{{Name: "README.md"}}}))
	files := plain(m)
	assert.Contains(t, files, "README.md")
	assert.NotContains(t, files, "Partial scan", "Files browsing ignores the Git discovery note")

	drain(m.SetMode(ModeChanges))
	changes := plain(m)
	require.Contains(t, changes, "sibling-0", "the Changes repository list is rendered")
	assert.Contains(t, changes, "Partial scan: repository limit", "Changes keeps the real incomplete warning (wrapped)")
}
