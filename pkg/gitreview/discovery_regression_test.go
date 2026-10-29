package gitreview

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// initEmptyCheckout creates the minimal Git metadata discovery recognizes without
// the commit cost of initRepo, so wide fixtures stay fast.
func initEmptyCheckout(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	gitTest(t, dir, "init", "--quiet", "-b", "main")
}

func repositoryNames(repos []Repository) map[string]bool {
	names := make(map[string]bool, len(repos))
	for _, repo := range repos {
		names[repo.Name] = true
	}
	return names
}

func TestDiscoverLevelOrderFindsShallowSiblingWithTinyBudget(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "a-noise", fmt.Sprintf("dir-%d", i)), 0o755))
	}
	initEmptyCheckout(t, filepath.Join(root, "z-repo"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	limits := discoveryLimits{depth: 4, entries: 4, repositories: MaxRepositories, timeout: time.Second}
	_, repos, reason, err := discover(t.Context(), root, nil, local, limits)
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitEntries, reason)
	require.True(t, repositoryNames(repos)["z-repo"], "a level-one sibling is visited before the children of an earlier container")
}

func TestLocalDiscoveryFindsSiblingsBeforeEarlyNonRepoContainerExhaustsBudget(t *testing.T) {
	root := t.TempDir()
	noisy := filepath.Join(root, "a-noise")
	for i := range DefaultDiscoveryEntries + 100 {
		require.NoError(t, os.MkdirAll(filepath.Join(noisy, fmt.Sprintf("dir-%05d", i)), 0o755))
	}
	for i := range 3 {
		initEmptyCheckout(t, filepath.Join(root, fmt.Sprintf("z-repo-%d", i)))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitEntries, snapshot.Scope.LimitReason, "the noisy container's own children exceed the budget and stay reported as partial")
	names := repositoryNames(snapshot.Repositories)
	for i := range 3 {
		require.True(t, names[fmt.Sprintf("z-repo-%d", i)], "sibling checkouts at the shallowest level are inventoried before deep noise in an earlier container")
	}
}

func TestLocalOwnedDiscoveryFindsDirectCheckoutAfterNoisyCandidate(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	noisy := filepath.Join(root, "a-noise")
	for i := range DefaultDiscoveryEntries + 100 {
		require.NoError(t, os.MkdirAll(filepath.Join(noisy, fmt.Sprintf("dir-%05d", i)), 0o755))
	}
	// Git lists a directory as one untracked candidate only if it has untracked files.
	require.NoError(t, os.WriteFile(filepath.Join(noisy, "untracked.txt"), nil, 0o600))
	initRepo(t, filepath.Join(root, "z-nearby"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.True(t, repositoryNames(snapshot.Repositories)["z-nearby"], "an explicit untracked checkout is not skipped because an earlier candidate exhausted the shared budget")
}

func TestLocalOwnedDiscoveryKeepsGroupedAndDeepCheckoutsWithinCap(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	for i := range 98 {
		initEmptyCheckout(t, filepath.Join(root, "g", fmt.Sprintf("r-%03d", i)))
	}
	initRepo(t, filepath.Join(root, "h", "deep", "x-repo"))
	initEmptyCheckout(t, filepath.Join(root, "z-top"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "98 grouped, one deep, and one top-level nested checkout plus the primary fit the 100-nested cap")
	require.Len(t, snapshot.Repositories, 101)
	names := repositoryNames(snapshot.Repositories)
	require.True(t, names["x-repo"], "a deeper candidate beside grouped candidates is still found")
	require.True(t, names["z-top"])
}

func TestLocalDiscoveryHundredSiblingsCompleteWithoutRepositoryWarning(t *testing.T) {
	root := t.TempDir()
	for i := range 100 {
		initEmptyCheckout(t, filepath.Join(root, fmt.Sprintf("repo-%03d", i)))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "exactly the default repository cap completes without a warning")
	require.Len(t, snapshot.Repositories, 101, "the synthetic workspace plus 100 siblings")
}

func TestLocalDiscoveryHundredTwentySiblingsReportsCapThenContinues(t *testing.T) {
	root := t.TempDir()
	for i := range 120 {
		initEmptyCheckout(t, filepath.Join(root, fmt.Sprintf("repo-%03d", i)))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	first, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitRepositories, first.Scope.LimitReason)
	require.Equal(t, MaxRepositories, first.Scope.LimitValue)
	require.Len(t, first.Repositories, 101, "the synthetic workspace plus the default cap of 100 siblings")
	require.True(t, local.CanContinueDiscovery())

	continued, err := local.ContinueDiscovery(t.Context())
	require.NoError(t, err)
	require.Greater(t, continued.Generation, first.Generation)
	require.Empty(t, continued.Scope.LimitReason)
	require.Len(t, continued.Repositories, 121)
}

func TestLocalDiscoveryGroupedLinkedWorktreesStayUnderOneRepository(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "linked-main")
	initRepo(t, main)
	group := filepath.Join(root, "group")
	for i := range 97 {
		initEmptyCheckout(t, filepath.Join(group, fmt.Sprintf("r-%03d", i)))
	}
	for i := range 3 {
		gitTest(t, main, "worktree", "add", "--quiet", "-b", fmt.Sprintf("wt-%d", i), filepath.Join(group, fmt.Sprintf("wt-%d", i)))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "linked worktree .git files do not consume the repository cap")
	require.Len(t, snapshot.Repositories, 99, "the synthetic workspace, the primary checkout, and 97 grouped checkouts")
	var linked int
	for _, repo := range snapshot.Repositories {
		if repo.Name != "linked-main" {
			continue
		}
		require.Len(t, repo.Worktrees, 4, "the primary and three linked worktrees are one repository")
		for _, wt := range repo.Worktrees {
			if wt.Linked {
				linked++
			}
		}
	}
	require.Equal(t, 3, linked)
}

func TestLocalOwnedDiscoveryFindsGroupedCheckoutBesideWideCandidate(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	noisy := filepath.Join(root, "a-noise")
	for i := range DefaultDiscoveryEntries + 200 {
		require.NoError(t, os.MkdirAll(filepath.Join(noisy, fmt.Sprintf("dir-%05d", i)), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(noisy, "untracked.txt"), nil, 0o600))
	initEmptyCheckout(t, filepath.Join(root, "z-parent", "r"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitEntries, snapshot.Scope.LimitReason, "the wide candidate still exhausts the budget and is reported")
	require.True(t, repositoryNames(snapshot.Repositories)["r"], "a same-depth group gets its round-robin turn before the wide candidate consumes the budget")
}

func TestLocalOwnedDiscoveryKeepsShallowCheckoutBeforeRepositoryCap(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	src := filepath.Join(root, "src")
	require.NoError(t, os.Mkdir(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("x\n"), 0o600))
	gitTest(t, root, "add", "src")
	gitTest(t, root, "commit", "--quiet", "-m", "tracked src")
	for i := range MaxRepositories {
		initEmptyCheckout(t, filepath.Join(src, fmt.Sprintf("r-%03d", i)))
	}
	initEmptyCheckout(t, filepath.Join(root, "zz-top"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitRepositories, snapshot.Scope.LimitReason)
	require.True(t, repositoryNames(snapshot.Repositories)["zz-top"], "a shallower checkout is not hidden by deeper lexically earlier checkouts")
}
