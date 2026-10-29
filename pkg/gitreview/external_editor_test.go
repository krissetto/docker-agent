package gitreview

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocalEditablePathOnlyResolvesPermittedRegularFiles(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	scope := filepath.Join(repo, "allowed")
	require.NoError(t, os.MkdirAll(scope, 0o755))
	inside := filepath.Join(scope, "résumé with spaces.txt")
	require.NoError(t, os.WriteFile(inside, []byte("safe\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("outside\n"), 0o600))
	require.NoError(t, os.Symlink(inside, filepath.Join(scope, "link.txt")))
	require.NoError(t, os.Mkdir(filepath.Join(scope, "directory"), 0o755))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	worktreeID := snapshot.Repositories[0].Worktrees[0].ID

	got, err := local.EditablePath(worktreeID, "résumé with spaces.txt")
	require.NoError(t, err)
	gotInfo, err := os.Stat(got)
	require.NoError(t, err)
	insideInfo, err := os.Stat(inside)
	require.NoError(t, err)
	require.True(t, os.SameFile(insideInfo, gotInfo), "resolved path should identify the scoped file")
	_, err = local.EditablePath(worktreeID, "../outside.txt")
	require.ErrorIs(t, err, ErrOutsideScope)
	_, err = local.EditablePath(worktreeID, "link.txt")
	require.ErrorIs(t, err, ErrOutsideScope)
	_, err = local.EditablePath(worktreeID, "directory")
	require.ErrorIs(t, err, ErrOutsideScope)
	_, err = local.EditablePath(worktreeID, "deleted.txt")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = local.EditablePath("unknown", "résumé with spaces.txt")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLocalEditablePathRejectsSymlinkedParentAndSubdirectoryScopeEscape(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	scope := filepath.Join(repo, "allowed")
	require.NoError(t, os.MkdirAll(scope, 0o755))
	outside := filepath.Join(repo, "elsewhere")
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(scope, "redirect")))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	_, err = local.EditablePath(snapshot.Repositories[0].Worktrees[0].ID, "redirect/file.txt")
	require.ErrorIs(t, err, ErrOutsideScope)
}

func TestParseWholeFileIndicators(t *testing.T) {
	patch := []byte("diff --git a/file b/file\n--- a/file\n+++ b/file\n@@ -1,2 +1,3 @@\n-old one\n-old two\n+new one\n+new two\n+new three\n@@ -8 +9,0 @@\n-old tail\n")
	require.Equal(t, []DocumentIndicator{
		{Line: 1, Kind: ChangeModified},
		{Line: 2, Kind: ChangeModified},
		{Line: 3, Kind: ChangeAdded},
		{Line: 9, Kind: ChangeDeleted},
	}, parseWholeFileIndicators(patch))
	require.Equal(t, []DocumentIndicator{{Line: 1, Kind: ChangeDeleted}}, parseWholeFileIndicators([]byte("@@ -1 +0,0 @@\n")))
}

func TestLocalFilesDocumentIncludesWholeFileIndicatorsWithoutSyntheticLines(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("one\nchanged\nthree\n"), 0o600))
	untracked := filepath.Join(root, "new file.txt")
	require.NoError(t, os.WriteFile(untracked, []byte("new\nline"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	id := snapshot.Repositories[0].Worktrees[0].ID

	modified, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: id, Path: "tracked.txt"})
	require.NoError(t, err)
	require.Equal(t, []DocumentIndicator{
		{Line: 2, Kind: ChangeModified},
		{Line: 3, Kind: ChangeAdded},
	}, modified.Indicators)
	require.Equal(t, []DocumentLine{
		{Kind: DiffContext, Old: 1, New: 1, Text: "one"},
		{Kind: DiffContext, Old: 2, New: 2, Text: "changed"},
		{Kind: DiffContext, Old: 3, New: 3, Text: "three"},
	}, modified.Lines, "indicators must not inject synthetic content lines")

	added, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: id, Path: "new file.txt"})
	require.NoError(t, err)
	require.Equal(t, []DocumentIndicator{
		{Line: 1, Kind: ChangeAdded},
		{Line: 2, Kind: ChangeAdded},
	}, added.Indicators)

	require.NoError(t, os.WriteFile(tracked, []byte("one\nthree\n"), 0o600))
	_, err = local.RefreshWorktree(t.Context(), id)
	require.NoError(t, err)
	deleted, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: id, Path: "tracked.txt"})
	require.NoError(t, err)
	require.Equal(t, []DocumentIndicator{{Line: 2, Kind: ChangeModified}}, deleted.Indicators,
		"a same-position replacement is an edit, not a pure deletion")

	require.NoError(t, os.WriteFile(tracked, []byte("one\n"), 0o600))
	_, err = local.RefreshWorktree(t.Context(), id)
	require.NoError(t, err)
	deletedAdjacency, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: id, Path: "tracked.txt"})
	require.NoError(t, err)
	require.Contains(t, deletedAdjacency.Indicators, DocumentIndicator{Line: 1, Kind: ChangeDeleted})
}

func TestLocalFilesDocumentHandlesUnbornRepoAndPlainWorkspace(t *testing.T) {
	unborn := t.TempDir()
	gitTest(t, unborn, "init", "--quiet", "-b", "main")
	path := filepath.Join(unborn, "first.txt")
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond"), 0o600))
	gitTest(t, unborn, "add", "first.txt")
	local, err := NewLocal(unborn)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	doc, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: snapshot.Repositories[0].Worktrees[0].ID, Path: "first.txt"})
	require.NoError(t, err, "an unborn HEAD must not break readable whole-file contents")
	require.Equal(t, []DocumentIndicator{{Line: 1, Kind: ChangeAdded}, {Line: 2, Kind: ChangeAdded}}, doc.Indicators)

	plain := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(plain, "note.txt"), []byte("plain text\n"), 0o600))
	plainLocal, err := NewLocal(plain)
	require.NoError(t, err)
	plainSnapshot, err := plainLocal.Snapshot(t.Context())
	require.NoError(t, err)
	plainDoc, err := plainLocal.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: plainSnapshot.Repositories[0].Worktrees[0].ID, Path: "note.txt"})
	require.NoError(t, err)
	require.Empty(t, plainDoc.Indicators, "non-Git workspace files have no Git change markers")
}

func TestDiscoverMetadataKeepsBoundedPrefixResults(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	nested := filepath.Join(root, "a-nested")
	initRepo(t, nested)
	for i := range 3 {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "z-untracked", fmt.Sprintf("dir-%d", i)), 0o755))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	owner, err := openCheckout(root)
	require.NoError(t, err)
	_, repos, limit, err := discover(t.Context(), root, owner, local, discoveryLimits{depth: 2, entries: 1, repositories: 10, timeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitEntries, limit)
	found := false
	for _, repo := range repos {
		if repo.Name == "a-nested" {
			found = true
		}
	}
	require.True(t, found, "bounded prefix repositories are retained even when later candidates are omitted")
}

func TestDiscoverMetadataSkipsCandidatesWithHiddenComponents(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	hiddenNested := filepath.Join(root, ".hidden", "nested")
	initRepo(t, hiddenNested)
	gitTest(t, root, "update-index", "--add", "--cacheinfo", "160000,"+gitTest(t, hiddenNested, "rev-parse", "HEAD")+",.hidden/nested")
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1, "all path components, not just the final name, must be pruned")
}
