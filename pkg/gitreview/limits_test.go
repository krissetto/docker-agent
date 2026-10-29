package gitreview

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLimitedBufferCapsWriteAndReadFrom(t *testing.T) {
	var direct limitedBuffer
	direct.limit = 16
	written, err := direct.Write([]byte(strings.Repeat("x", 64)))
	require.ErrorIs(t, err, ErrTooLarge)
	require.Equal(t, 16, written)
	require.Len(t, direct.Bytes(), 16)
	require.True(t, direct.overflow)

	var copied limitedBuffer
	copied.limit = 16
	n, err := copied.ReadFrom(strings.NewReader(strings.Repeat("y", 64<<10)))
	require.ErrorIs(t, err, ErrTooLarge)
	require.EqualValues(t, 16, n)
	require.Len(t, copied.Bytes(), 16)
	require.True(t, copied.overflow)
}

func TestLimitedBufferReadFromHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := cancelReader{done: ctx.Done()}
	var bounded limitedBuffer
	bounded.limit = 1 << 20
	done := make(chan error, 1)
	go func() {
		_, err := bounded.ReadFrom(reader)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("ReadFrom did not stop after context cancellation")
	}
}

type cancelReader struct{ done <-chan struct{} }

func (r cancelReader) Read([]byte) (int, error) {
	<-r.done
	return 0, context.Canceled
}

func TestLocalSnapshotCorruptIndexKeepsOwnerAndReportsMetadataUnavailable(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "index"), []byte("garbage"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1)
	worktreeID := snapshot.Repositories[0].Worktrees[0].ID
	directory, err := local.Directory(t.Context(), worktreeID, ".")
	require.NoError(t, err)
	require.NotEmpty(t, directory.Entries, "folder remains browseable despite the corrupted index")
	doc, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: worktreeID, Path: "tracked.txt"})
	require.NoError(t, err, "file contents remain readable despite the corrupted index")
	require.Contains(t, doc.Lines, DocumentLine{Kind: DiffContext, Old: 1, New: 1, Text: "one"})
}

func largeIndexForTest(t *testing.T, root string, count int) {
	t.Helper()
	blob := gitTest(t, root, "hash-object", "-w", "/dev/null")
	var indexInfo strings.Builder
	for i := range count {
		_, _ = fmt.Fprintf(&indexInfo, "100644 %s\tgenerated/module-%05d/component-with-a-long-descriptive-name-%05d.go\n", blob, i/100, i)
	}
	cmd := exec.Command("git", "-C", root, "update-index", "--index-info")
	cmd.Stdin = strings.NewReader(indexInfo.String())
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestLocalMetadataOutputCapNoLongerCountsAllTrackedIndexEntries(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	largeIndexForTest(t, root, 20_000)
	local, err := NewLocal(root)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	snapshot, err := local.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "ordinary tracked index entries no longer consume nested-discovery output budget")
	require.Len(t, snapshot.Repositories, 1)
}

func TestLocalMetadataLimitRetainsNestedRepoFoundBeforeCandidateBound(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	initRepo(t, filepath.Join(root, "a-nested"))
	for i := range DefaultDiscoveryEntries + 1 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "z-untracked", fmt.Sprintf("dir-%05d", i)), 0o755))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	found := false
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "a-nested"
	}
	require.True(t, found)
	require.Equal(t, DiscoveryLimitEntries, snapshot.Scope.LimitReason)
}

func TestLocalRepositoryLimitRequiresAnotherDistinctRepository(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	initRepo(t, first)
	local, err := NewLocal(parent)
	require.NoError(t, err)
	limits := discoveryLimits{depth: 4, entries: 128, repositories: 1, timeout: time.Second}
	_, repos, reason, err := discover(t.Context(), parent, nil, local, limits)
	require.NoError(t, err)
	require.Len(t, repos, 2, "the synthetic workspace and exactly one Git checkout are complete")
	require.Empty(t, reason, "filling but not exceeding the repository cap is complete")

	initRepo(t, filepath.Join(parent, "second"))
	_, repos, reason, err = discover(t.Context(), parent, nil, local, limits)
	require.NoError(t, err)
	require.Len(t, repos, 2, "the first distinct checkout is retained")
	require.Equal(t, DiscoveryLimitRepositories, reason, "only another distinct checkout makes the result partial")
}

func TestLocalUntrackedMetadataByteLimitKeepsRepositoriesInCapturedPrefix(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	nestedName := "a-nested"
	initRepo(t, filepath.Join(root, nestedName))
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	shim := `#!/bin/sh
case "$*" in
  *"--others --directory --exclude-standard -z"*)
    printf '%s/\0' "$NESTED_NAME"
    head -c 2097153 /dev/zero
    exit 0
    ;;
esac
exec "$REAL_GIT" "$@"
`
	gitPath := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(gitPath, []byte(shim), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NESTED_NAME", nestedName)
	t.Setenv("REAL_GIT", realGit)
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitBytes, snapshot.Scope.LimitReason)
	found := false
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == nestedName
	}
	require.True(t, found, "bounded output keeps complete candidates preceding a truncated record")
}

func TestLocalDiscoveryFindsManyShallowSiblingRepositoriesWithoutWalkingIgnoredTrees(t *testing.T) {
	root := t.TempDir()
	const siblings = 48
	for i := range siblings {
		repo := filepath.Join(root, fmt.Sprintf("repo-%02d", i))
		initRepo(t, repo)
		tracked := filepath.Join(repo, "tracked-tree")
		require.NoError(t, os.MkdirAll(tracked, 0o755))
		for j := range 100 {
			require.NoError(t, os.WriteFile(filepath.Join(tracked, fmt.Sprintf("file-%03d.txt", j)), nil, 0o600))
		}
		require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored-cache/\n"), 0o600))
		gitTest(t, repo, "add", "tracked-tree", ".gitignore")
		gitTest(t, repo, "commit", "--quiet", "-m", "large tracked tree")
		ignored := filepath.Join(repo, "ignored-cache")
		for j := range 80 {
			dir := filepath.Join(ignored, fmt.Sprintf("deep-%03d", j), "more")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "payload"), nil, 0o600))
		}
		untracked := filepath.Join(repo, "untracked-tree")
		require.NoError(t, os.MkdirAll(untracked, 0o755))
		for j := range 100 {
			require.NoError(t, os.WriteFile(filepath.Join(untracked, fmt.Sprintf("file-%03d.txt", j)), nil, 0o600))
		}
	}

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	trace := filepath.Join(t.TempDir(), "trace")
	shim := "#!/bin/sh\nprintf x >> \"$GIT_REVIEW_DISCOVERY_TRACE\"\nexec \"$REAL_GIT\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("GIT_REVIEW_DISCOVERY_TRACE", trace)

	local, err := NewLocal(root)
	require.NoError(t, err)
	started := time.Now()
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Less(t, time.Since(started), 5*time.Second)
	require.Len(t, snapshot.Repositories, siblings+1)
	require.Empty(t, snapshot.Scope.LimitReason)
	_, err = os.Stat(trace)
	require.ErrorIs(t, err, os.ErrNotExist, "a parent workspace inventories Git markers without spawning per-repository Git commands")
}

func TestLocalDiscoveryUsesBoundedGitmodulesPathsForSubmodules(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	source := filepath.Join(t.TempDir(), "component-source")
	initRepo(t, source)
	gitTest(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", source, "deps/component")
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 2)
	var found bool
	for _, repo := range snapshot.Repositories {
		if repo.Name == "component" {
			found = true
			require.Equal(t, KindSubmodule, repo.Kind)
		}
	}
	require.True(t, found, "tracked submodules are enumerated from bounded .gitmodules metadata")
}

func TestLocalDiscoveryOrdinaryFilesDoNotConsumeDirectoryEntryBudget(t *testing.T) {
	root := t.TempDir()
	fileDir := filepath.Join(root, "a-many-files")
	require.NoError(t, os.Mkdir(fileDir, 0o755))
	for i := range DefaultDiscoveryEntries + 100 {
		require.NoError(t, os.WriteFile(filepath.Join(fileDir, fmt.Sprintf("file-%05d", i)), nil, 0o600))
	}
	initRepo(t, filepath.Join(root, "z-nearby-repo"))

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "ordinary files do not consume the directory-discovery budget")
	require.Len(t, snapshot.Repositories, 2)
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "z-nearby-repo"
	}
	require.True(t, found, "the nearby checkout after a file-heavy directory remains discoverable")
}

func TestLocalOwnedDiscoveryIgnoresOrdinaryUntrackedFiles(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	for i := range DefaultDiscoveryEntries + 100 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("untracked-%05d.txt", i)), nil, 0o600))
	}
	initRepo(t, filepath.Join(root, "nearby-repo"))

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason, "ordinary untracked files do not consume the directory-candidate budget")
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "nearby-repo"
	}
	require.True(t, found, "the nearby nested repository remains discoverable")
}

func TestLocalOwnedDiscoveryContinuesAfterDepthLimitedCandidate(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	deep := filepath.Join(root, "a-deep")
	for i := range DefaultDiscoveryDepth + 1 {
		deep = filepath.Join(deep, fmt.Sprintf("level-%02d", i))
		require.NoError(t, os.MkdirAll(deep, 0o755))
	}
	initRepo(t, filepath.Join(deep, "too-deep"))
	nearby := filepath.Join(root, "z-nearby", "repo")
	initRepo(t, nearby)

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitDepth, snapshot.Scope.LimitReason)
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "repo"
	}
	require.True(t, found, "a depth-limited candidate must not suppress later eligible peer candidates")
}

func TestLocalSubdirectoryDiscoveryStaysScopeRelative(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	scope := filepath.Join(root, "allowed")
	require.NoError(t, os.MkdirAll(filepath.Join(scope, "nested"), 0o755))
	initRepo(t, filepath.Join(scope, "nested", "repo"))
	initRepo(t, filepath.Join(scope, "ignored", "repo"))
	require.NoError(t, os.WriteFile(filepath.Join(scope, ".agentsignore"), []byte("ignored/\n"), 0o600))
	initRepo(t, filepath.Join(root, "outside", "repo"))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	var found, outside, ignored bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "repo" && len(repo.Worktrees) > 0 && repo.Worktrees[0].Path == filepath.Join("nested", "repo")
		outside = outside || repo.Name == "outside"
		ignored = ignored || len(repo.Worktrees) > 0 && strings.Contains(repo.Worktrees[0].Path, "ignored")
	}
	require.True(t, found, "nested repositories below the bound subdirectory remain discoverable")
	require.False(t, outside, "repository candidates outside the immutable scope are excluded")
	require.False(t, ignored, "scope-root .agentsignore is respected for an enclosing checkout")
}

func TestLocalDiscoveryContinuationStopsAtDepthEight(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	path := root
	for i := range MaxDiscoveryDepth {
		path = filepath.Join(path, fmt.Sprintf("level-%02d", i))
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	initRepo(t, filepath.Join(path, "too-deep"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitDepth, snapshot.Scope.LimitReason)
	for depth := DefaultDiscoveryDepth + 2; depth <= MaxDiscoveryDepth; depth += 2 {
		snapshot, err = local.ContinueDiscovery(t.Context())
		require.NoError(t, err)
		require.Equal(t, depth, snapshot.Scope.LimitValue)
	}
	require.Equal(t, DiscoveryLimitDepth, snapshot.Scope.LimitReason)
	require.False(t, local.CanContinueDiscovery())
	require.Len(t, snapshot.Repositories, 1, "the repository at depth nine stays outside the explicit depth-eight cap")
}

func TestLocalSubdirectoryDiscoverySupportsLiteralGlobCharacters(t *testing.T) {
	parent := t.TempDir()
	initRepo(t, parent)
	scope := filepath.Join(parent, "project[1]")
	require.NoError(t, os.MkdirAll(scope, 0o755))
	initRepo(t, filepath.Join(scope, "nearby"))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason)
	var found bool
	for _, discovered := range snapshot.Repositories {
		found = found || discovered.Name == "nearby"
	}
	require.True(t, found, "literal metacharacters in the scope path do not corrupt Git's directory metadata pathspec")
}

func TestLocalOwnedDirectoryMetadataIncludesNestedRepoBelowTrackedDirectories(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked")
	require.NoError(t, os.MkdirAll(tracked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tracked, "file.txt"), []byte("tracked\n"), 0o600))
	gitTest(t, root, "add", "tracked")
	gitTest(t, root, "commit", "--quiet", "-m", "tracked directory")
	initRepo(t, filepath.Join(tracked, "nested", "repo"))
	if err := os.Mkdir(filepath.Join(root, "empty-ordinary-dir"), 0o755); err != nil {
		require.NoError(t, err)
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "repo"
	}
	require.True(t, found, "directory-only Git metadata includes nested repos below tracked/index-managed parents")
	require.Empty(t, snapshot.Scope.LimitReason, "ordinary empty directories do not create a partial discovery report")
}

func TestLocalOwnedDiscoveryIgnoresIncompleteFinalMetadataRecord(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	initRepo(t, filepath.Join(root, "a-complete"))
	initRepo(t, filepath.Join(root, "z-truncated"))
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	shim := `#!/bin/sh
case "$*" in
  *"ls-files"*"--others"*"--directory"*)
    printf 'a-complete/\0z-truncated/'
    exit 0
    ;;
esac
exec "$REAL_GIT" "$@"
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REAL_GIT", realGit)
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitUnavailable, snapshot.Scope.LimitReason, "malformed metadata records must not imply a complete inventory")
	var complete, truncated bool
	for _, repo := range snapshot.Repositories {
		complete = complete || repo.Name == "a-complete"
		truncated = truncated || repo.Name == "z-truncated"
	}
	require.True(t, complete, "the valid metadata prefix remains useful")
	require.False(t, truncated, "the unterminated directory record is never trusted")
}

func TestLocalSubdirectoryDiscoveryFindsRepositoriesBelowUntrackedScopeRoot(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	scope := filepath.Join(root, "untracked-scope")
	initRepo(t, filepath.Join(scope, "nearby"))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason)
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "nearby"
	}
	require.True(t, found, "Git's ./ record for an untracked scope root still triggers bounded child discovery")
}
