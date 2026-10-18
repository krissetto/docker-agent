package gitreview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	gitTest(t, dir, "init", "--quiet", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.invalid")
	gitTest(t, dir, "config", "user.name", "Git Review Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\ntwo\n"), 0o600))
	gitTest(t, dir, "add", "tracked.txt")
	gitTest(t, dir, "commit", "--quiet", "-m", "initial")
}

func TestLocalSnapshotWorktreeStatusAndLazyDocuments(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("one\nchanged\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o600))
	local, err := NewLocal(repo)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1)
	require.Len(t, snapshot.Repositories[0].Worktrees, 1)
	wt := snapshot.Repositories[0].Worktrees[0]
	require.Equal(t, "unscanned", wt.State, "discovery must not eagerly run status")
	loaded, err := local.LoadWorktree(t.Context(), wt.ID)
	require.NoError(t, err)
	require.Equal(t, "changed", loaded.State)
	require.Len(t, loaded.Files, 2)
	var modified, untracked FileChange
	for _, f := range loaded.Files {
		if f.Path == "tracked.txt" {
			modified = f
		}
		if f.Path == "new.txt" {
			untracked = f
		}
	}
	require.Equal(t, GroupUnstaged, modified.Group)
	require.Equal(t, GroupUntracked, untracked.Group)
	doc, err := local.Document(t.Context(), DocumentKey{Mode: "changes", WorktreeID: wt.ID, Path: "tracked.txt", Group: GroupUnstaged})
	require.NoError(t, err)
	require.Equal(t, StateReady, doc.State)
	require.NotEmpty(t, doc.Lines)
	require.Contains(t, doc.Lines, DocumentLine{Kind: DiffRemove, Old: 2, Text: "two"})
	require.Contains(t, doc.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "changed"})
	untrackedDoc, err := local.Document(t.Context(), DocumentKey{Mode: "changes", WorktreeID: wt.ID, Path: "new.txt", Group: GroupUntracked})
	require.NoError(t, err)
	require.Equal(t, StateReady, untrackedDoc.State)
	require.Contains(t, untrackedDoc.Lines, DocumentLine{Kind: DiffAdd, New: 1, Text: "new"})
}

func TestLocalScopeSubdirectoryAndNestedRepoContainment(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	sub := filepath.Join(repo, "allowed")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sub, "inside.txt"), []byte("inside\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("outside\n"), 0o600))
	nested := filepath.Join(sub, "nested")
	initRepo(t, nested)
	local, err := NewLocal(sub)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 2, "owner and nested repository are distinct")
	var ownerWT string
	for _, repo := range snapshot.Repositories {
		for _, wt := range repo.Worktrees {
			if wt.Current {
				ownerWT = wt.ID
			}
		}
	}
	require.NotEmpty(t, ownerWT)
	_, err = local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: ownerWT, Path: "../outside.txt"})
	require.ErrorIs(t, err, ErrOutsideScope)
	_, err = local.Directory(t.Context(), ownerWT, "../")
	require.ErrorIs(t, err, ErrOutsideScope)
	link := filepath.Join(sub, "escape")
	require.NoError(t, os.Symlink(root, link))
	_, err = local.Directory(t.Context(), ownerWT, "escape")
	require.ErrorIs(t, err, ErrOutsideScope)
}

func TestLocalLinkedWorktreesUseCheckoutIdentityAndFilterOutside(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	initRepo(t, repo)
	inside := filepath.Join(repo, "checkouts", "in")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(filepath.Dir(inside), 0o755))
	gitTest(t, repo, "worktree", "add", "--quiet", "-b", "inside", inside)
	gitTest(t, repo, "worktree", "add", "--quiet", "-b", "outside", outside)
	require.NoError(t, os.WriteFile(filepath.Join(inside, "tracked.txt"), []byte("inside edit\n"), 0o600))
	local, err := NewLocal(repo)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1)
	require.Equal(t, 1, snapshot.Repositories[0].HiddenOutside)
	ids := map[string]bool{}
	for _, wt := range snapshot.Repositories[0].Worktrees {
		ids[wt.ID] = true
	}
	require.Len(t, ids, 2)
	var changed Worktree
	for id := range ids {
		wt, e := local.LoadWorktree(t.Context(), id)
		require.NoError(t, e)
		if wt.State == "changed" {
			changed = wt
		}
	}
	require.NotEmpty(t, changed.ID)
	var inWorktree Worktree
	for _, wt := range snapshot.Repositories[0].Worktrees {
		if wt.Branch == "inside" {
			inWorktree = wt
		}
	}
	require.NotEmpty(t, inWorktree.ID)
	require.True(t, inWorktree.Linked)
	require.NotEqual(t, "repo", inWorktree.Path)
	_, err = local.LoadWorktree(context.Background(), "../outside")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestParseStatusPorcelainV2RenameAndConflict(t *testing.T) {
	data := []byte("1 .M N... 100644 100644 100644 a b renamed.txt\x002 R. N... 100644 100644 100644 c d R100 new name.txt\x00old name.txt\x00u UU N... 100644 100644 100644 100644 c d e conflict.txt\x00")
	files := parseStatus(data)
	require.Len(t, files, 3)
	byPath := map[string]FileChange{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	require.Equal(t, GroupUnstaged, byPath["renamed.txt"].Group)
	require.Equal(t, "old name.txt", byPath["new name.txt"].OldPath)
	require.Equal(t, GroupConflicts, byPath["conflict.txt"].Group)
}

func TestParsePatchHunksAndBinary(t *testing.T) {
	key := DocumentKey{Mode: "changes", WorktreeID: "wt", Path: "a", Group: GroupStaged}
	doc := parsePatch(key, []byte("diff --git a/a b/a\nindex 1..2 100644\n--- a/a\n+++ b/a\n@@ -8,2 +8,2 @@ func f\n old\n+new\n"))
	require.Equal(t, 8, doc.Lines[0].Old)
	require.Equal(t, 8, doc.Lines[0].New)
	require.Equal(t, DiffContext, doc.Lines[1].Kind)
	binary := parsePatch(key, []byte("diff --git a/a b/a\nBinary files a/a and b/a differ\n"))
	require.True(t, binary.Binary)
}

func TestLocalDiscoveryAndDirectoryRespectFreshIgnoreRules(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored-dir/\n*.tmp\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".agentsignore"), []byte("private.txt\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "ignored-dir", "nested-repo"), 0o755))
	initRepo(t, filepath.Join(repo, "ignored-dir", "nested-repo"))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "visible.txt"), []byte("visible\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "skip.tmp"), []byte("ignored\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "private.txt"), []byte("secret\n"), 0o600))
	local, err := NewLocal(repo)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1, "ignored nested repositories are not discovered")
	wt := snapshot.Repositories[0].Worktrees[0]
	dir, err := local.Directory(t.Context(), wt.ID, ".")
	require.NoError(t, err)
	for _, entry := range dir.Entries {
		require.NotEqual(t, "ignored-dir", entry.Name)
		require.NotEqual(t, "private.txt", entry.Name)
	}
}

func TestLocalPlainWorkspaceFilesAndEmptyChanges(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1)
	require.Equal(t, KindNone, snapshot.Repositories[0].Kind)
	wt := snapshot.Repositories[0].Worktrees[0]
	doc, err := local.Document(t.Context(), DocumentKey{Mode: "files", WorktreeID: wt.ID, Path: "note.txt"})
	require.NoError(t, err)
	require.Equal(t, StateReady, doc.State)
	require.Contains(t, doc.Lines, DocumentLine{Kind: DiffContext, Old: 1, New: 1, Text: "hello"})
}

func TestLocalStagedDeletionDocumentRemainsReadable(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	gitTest(t, root, "rm", "--cached", "--quiet", "tracked.txt")
	require.NoError(t, os.Remove(filepath.Join(root, "tracked.txt")))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wt := snapshot.Repositories[0].Worktrees[0]
	status, err := local.LoadWorktree(t.Context(), wt.ID)
	require.NoError(t, err)
	require.Len(t, status.Files, 1)
	doc, err := local.Document(t.Context(), DocumentKey{Mode: "changes", WorktreeID: wt.ID, Path: "tracked.txt", Group: GroupStaged})
	require.NoError(t, err)
	require.Equal(t, StateReady, doc.State)
	require.NotEmpty(t, doc.Lines)
}

func TestLocalDiscoversNestedRepositoriesRecursively(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	first := filepath.Join(root, "one")
	initRepo(t, first)
	second := filepath.Join(first, "two")
	initRepo(t, second)
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 3)
}

func TestLocalRefreshWorktreeInvalidatesCachedStatusAndDocuments(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked.txt")
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wtID := snapshot.Repositories[0].Worktrees[0].ID

	require.NoError(t, os.WriteFile(tracked, []byte("one\nfirst edit\n"), 0o600))
	status, err := local.LoadWorktree(t.Context(), wtID)
	require.NoError(t, err)
	require.Equal(t, "changed", status.State)
	key := DocumentKey{Mode: "changes", WorktreeID: wtID, Path: "tracked.txt", Group: GroupUnstaged}
	first, err := local.Document(t.Context(), key)
	require.NoError(t, err)
	require.Contains(t, first.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "first edit"})

	require.NoError(t, os.WriteFile(tracked, []byte("one\nsecond edit\n"), 0o600))
	refreshed, err := local.RefreshWorktree(t.Context(), wtID)
	require.NoError(t, err)
	require.Equal(t, "changed", refreshed.State)
	second, err := local.Document(t.Context(), key)
	require.NoError(t, err)
	require.Contains(t, second.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "second edit"})
	require.NotContains(t, second.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "first edit"})
	require.Equal(t, snapshot.Generation, local.CurrentSnapshot().Generation, "targeted refresh must not change discovery generation")
}

func TestLocalDocumentRequestsCoalesceAndCanceledWaiterDoesNotCancelPeer(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("one\nchanged\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wtID := snapshot.Repositories[0].Worktrees[0].ID

	wrapper := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(wrapper, 0o700))
	trace := filepath.Join(t.TempDir(), "trace")
	shim := "#!/bin/sh\ncase \"$*\" in *--no-ext-diff*) printf x >> \"$GIT_REVIEW_PERF_TRACE\"; sleep 0.2;; esac\nexec /usr/bin/git \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapper, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", wrapper+":"+os.Getenv("PATH"))
	t.Setenv("GIT_REVIEW_PERF_TRACE", trace)

	key := DocumentKey{Mode: "changes", WorktreeID: wtID, Path: "tracked.txt", Group: GroupUnstaged}
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { _, e := local.Document(firstCtx, key); firstDone <- e }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, readErr := os.ReadFile(trace); readErr == nil && len(data) > 0 {
			break
		}
		require.Less(t, time.Now(), deadline, "document git command did not start")
		time.Sleep(time.Millisecond)
	}
	peerDone := make(chan error, 1)
	go func() { _, e := local.Document(t.Context(), key); peerDone <- e }()
	time.Sleep(20 * time.Millisecond)
	cancelFirst()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	require.NoError(t, <-peerDone)
	data, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Len(t, data, 1, "identical overlapping requests should run one git diff")
}

func TestLocalRefreshWorktreePreventsInflightDocumentFromRefillingCache(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("one\nfirst edit\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wtID := snapshot.Repositories[0].Worktrees[0].ID
	key := DocumentKey{Mode: "changes", WorktreeID: wtID, Path: "tracked.txt", Group: GroupUnstaged}

	wrapper := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(wrapper, 0o700))
	trace := filepath.Join(t.TempDir(), "trace")
	shim := "#!/bin/sh\ncase \"$*\" in *--no-ext-diff*) printf x >> \"$GIT_REVIEW_PERF_TRACE\"; sleep 0.3;; esac\nexec /usr/bin/git \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapper, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", wrapper+":"+os.Getenv("PATH"))
	t.Setenv("GIT_REVIEW_PERF_TRACE", trace)

	oldDone := make(chan error, 1)
	go func() { _, e := local.Document(t.Context(), key); oldDone <- e }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, readErr := os.ReadFile(trace); readErr == nil && len(data) > 0 {
			break
		}
		require.Less(t, time.Now(), deadline, "document git command did not start")
		time.Sleep(time.Millisecond)
	}
	require.NoError(t, os.WriteFile(tracked, []byte("one\nsecond edit\n"), 0o600))
	refreshed, err := local.RefreshWorktree(t.Context(), wtID)
	require.NoError(t, err)
	require.Equal(t, "changed", refreshed.State)
	require.Error(t, <-oldDone, "refresh cancels documents that started against the old worktree revision")
	fresh, err := local.Document(t.Context(), key)
	require.NoError(t, err)
	require.Contains(t, fresh.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "second edit"})
	require.NotContains(t, fresh.Lines, DocumentLine{Kind: DiffAdd, New: 2, Text: "first edit"})
}

func TestLocalWorktreeStatusCacheExpires(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wtID := snapshot.Repositories[0].Worktrees[0].ID
	first, err := local.LoadWorktree(t.Context(), wtID)
	require.NoError(t, err)
	require.Equal(t, "clean", first.State)
	require.NoError(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("external edit\n"), 0o600))
	time.Sleep(statusCacheTTL + 20*time.Millisecond)
	second, err := local.LoadWorktree(t.Context(), wtID)
	require.NoError(t, err)
	require.Equal(t, "changed", second.State)
}

func TestLocalWorktreeLoadsCoalesceAndCanceledWaiterDoesNotCancelPeer(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	wtID := snapshot.Repositories[0].Worktrees[0].ID

	wrapper := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.MkdirAll(wrapper, 0o700))
	trace := filepath.Join(t.TempDir(), "trace")
	shim := "#!/bin/sh\nprintf x >> \"$GIT_REVIEW_PERF_TRACE\"\nsleep 0.15\nexec /usr/bin/git \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(wrapper, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", wrapper+":"+os.Getenv("PATH"))
	t.Setenv("GIT_REVIEW_PERF_TRACE", trace)

	ctx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { _, loadErr := local.LoadWorktree(ctx, wtID); firstDone <- loadErr }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, readErr := os.ReadFile(trace); readErr == nil && len(data) > 0 {
			break
		}
		require.Less(t, time.Now(), deadline, "status git command did not start")
		time.Sleep(time.Millisecond)
	}
	peerDone := make(chan error, 1)
	go func() { _, loadErr := local.LoadWorktree(t.Context(), wtID); peerDone <- loadErr }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	require.NoError(t, <-peerDone)
	data, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(string(data), "x"), "three status/binary Git commands should run once for both callers")
}
