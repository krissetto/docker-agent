package sidebar

import (
	"strconv"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/review"
)

// filesDiscoverySnapshot models a bounded Git discovery that found the owner
// checkout plus many sibling repositories, none of which are browsed yet.
func filesDiscoverySnapshot(siblings int) *review.Snapshot {
	snap := &review.Snapshot{Generation: 3, Scope: review.Scope{
		LimitReason: "repositories", LimitValue: 100,
		Note: "Partial scan: repository limit reached (100).",
	}}
	snap.Repositories = append(snap.Repositories, review.Repository{ID: "owner", Name: "workspace", Kind: review.RepoPrimary, Worktrees: []review.Worktree{
		{ID: "owner-wt", Path: ".", Current: true, Branch: "main", State: review.StateReady, CountsState: review.StateReady},
	}})
	for i := range siblings {
		id := "sibling-" + strconv.Itoa(i)
		snap.Repositories = append(snap.Repositories, review.Repository{ID: id, Name: id, Kind: review.RepoNested, Worktrees: []review.Worktree{
			{ID: id + "-wt", Path: "repos/" + id, Branch: "main", State: review.StateUnscanned},
		}})
	}
	return snap
}

func TestFilesIgnoresGlobalDiscoveryLimitWhileGitKeepsIt(t *testing.T) {
	m := gitSidebar(t)
	snap := filesDiscoverySnapshot(120)
	dirs := []review.Directory{{WorktreeID: "owner-wt", State: review.StateReady, Entries: []review.Entry{{Name: "README.md"}}}}
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snap, Mode: review.ModeFiles, ContinueDiscovery: true, DiscoveryContinuationSupported: true, Directories: dirs})

	files := gitView(m)
	assert.Contains(t, files, "README.md", "the requested root listing is shown")
	assert.NotContains(t, files, "Incomplete", "Files browsing is not a discovery status surface")
	assert.NotContains(t, files, "Continue scan")
	assert.NotContains(t, files, "Partial scan")

	// Only the scope root is requested; none of the 120 collapsed sibling
	// repositories enqueue a directory listing.
	assert.Empty(t, drainGit(m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snap, Mode: review.ModeFiles, Directories: dirs})))

	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snap, Mode: review.ModeChanges, ContinueDiscovery: true, DiscoveryContinuationSupported: true})
	changes := gitView(m)
	assert.Contains(t, changes, "Incomplete · repositories 100", "Git keeps the real discovery warning")
	assert.Contains(t, changes, "Continue scan")
}

func TestFilesExpansionRequestsOnlyTheOpenedDirectory(t *testing.T) {
	m := gitSidebar(t)
	snap := filesDiscoverySnapshot(120)
	dirs := []review.Directory{{WorktreeID: "owner-wt", State: review.StateReady, Entries: []review.Entry{{Name: "docs", Dir: true}}}}
	m.git.expanded = map[string]bool{gitSectionKey: true}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snap, Mode: review.ModeFiles, Directories: dirs})

	line := gitLine(m, "docs/")
	require.NotEmpty(t, line)
	requests := drainGit(m.ActivateGitLine(line))
	require.Equal(t, []GitRequestMsg{{Owner: "page", Action: GitLoadDirectory, WorktreeID: "owner-wt", Path: "docs"}}, requests,
		"expanding one folder is one ls-like request, not a tree walk")
}

func TestFilesShowsOnlyOwnDirectoryTruncationAndErrors(t *testing.T) {
	m := gitSidebar(t)
	snap := filesDiscoverySnapshot(120)
	dirs := []review.Directory{
		{WorktreeID: "owner-wt", State: review.StateReady, Entries: []review.Entry{{Name: "docs", Dir: true}, {Name: "locked", Dir: true}}},
		{WorktreeID: "owner-wt", Path: "docs", State: review.StateReady, Truncated: true, Entries: []review.Entry{{Name: "a.md"}}},
		{WorktreeID: "owner-wt", Path: "locked", State: review.StateError, Err: "permission denied"},
	}
	m.git.expanded = map[string]bool{gitSectionKey: true, browseDirKey("owner-wt", "docs"): true, browseDirKey("owner-wt", "locked"): true}
	m.SetGitChanges(GitChanges{Owner: "page", Available: true, Snapshot: snap, Mode: review.ModeFiles, Directories: dirs})

	view := ansi.Strip(gitView(m))
	assert.Contains(t, view, "More entries not shown", "a truncated folder says its listing is partial")
	assert.Contains(t, view, "permission denied", "a failed folder shows its own error")
	assert.NotContains(t, view, "Incomplete", "own directory state does not inherit the global discovery limit")
}
