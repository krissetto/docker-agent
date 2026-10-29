package gitreview

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestLocalLinkedCheckoutDiscoveredFirstUsesCanonicalMainAndOrdersMainFirst(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "z-main-project")
	initRepo(t, main)
	linked := filepath.Join(root, "a-linked-checkout")
	gitTest(t, main, "worktree", "add", "--quiet", "-b", "linked", linked)

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	var repo *Repository
	for i := range snapshot.Repositories {
		if snapshot.Repositories[i].ID != "" && snapshot.Repositories[i].Name == "z-main-project" {
			repo = &snapshot.Repositories[i]
		}
	}
	require.NotNil(t, repo, "the linked checkout encountered first groups under the real main repository")
	require.Len(t, repo.Worktrees, 2)
	require.False(t, repo.Worktrees[0].Linked, "the main checkout is the stable top-level child")
	require.Equal(t, "z-main-project", repo.Worktrees[0].Path)
	require.True(t, repo.Worktrees[1].Linked)
	require.Equal(t, "a-linked-checkout", repo.Worktrees[1].Path)
	for _, candidate := range snapshot.Repositories {
		require.NotEqual(t, "a-linked-checkout", candidate.Name, "linked path never becomes repository identity")
	}
}

func TestLocalLinkedWorktreeFirstKeepsCanonicalMainNameAndSafeScope(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "canonical-project")
	initRepo(t, main)
	linked := filepath.Join(base, "linked-checkout")
	gitTest(t, main, "worktree", "add", "--quiet", "-b", "feature", linked)

	local, err := NewLocal(linked)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1)
	repo := snapshot.Repositories[0]
	require.Equal(t, "canonical-project", repo.Name, "repository identity comes from the main checkout even when the linked checkout is the scope")
	require.Equal(t, 1, repo.HiddenOutside, "the out-of-scope main checkout is counted but not exposed")
	require.Len(t, repo.Worktrees, 1)
	require.Equal(t, ".", repo.Worktrees[0].Path)
	require.True(t, repo.Worktrees[0].Linked)
	require.True(t, repo.Worktrees[0].Current)
	require.NotContains(t, repo.Worktrees[0].Path, filepath.Base(main))
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

func TestLocalDiscoversNestedRepositoriesWithoutDescendingIntoThem(t *testing.T) {
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
	require.Len(t, snapshot.Repositories, 2)
	require.Empty(t, snapshot.Scope.LimitReason, "repository internals are separate scope, not a partial scan")
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

func TestLocalWorktreeLineCountsCoverGroupsUntrackedBinaryAndRenames(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	tracked := filepath.Join(root, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("one\nstaged\n"), 0o600))
	gitTest(t, root, "add", "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("one\nstaged\nworking\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "new file.txt"), []byte("first\nsecond"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "image.bin"), []byte{0, 1, 2}, 0o600))

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	loaded, err := local.LoadWorktree(t.Context(), snapshot.Repositories[0].Worktrees[0].ID)
	require.NoError(t, err)
	require.Equal(t, StateReady, loaded.CountsState)
	byKey := map[string]FileChange{}
	for _, file := range loaded.Files {
		byKey[string(file.Group)+":"+file.Path] = file
	}
	require.Equal(t, FileChange{Path: "tracked.txt", Group: GroupStaged, Status: "M", Added: 1, Removed: 1}, byKey["staged:tracked.txt"])
	require.Equal(t, FileChange{Path: "tracked.txt", Group: GroupUnstaged, Status: "M", Added: 1, Removed: 0}, byKey["unstaged:tracked.txt"])
	require.Equal(t, 2, byKey["untracked:new file.txt"].Added)
	require.Equal(t, 0, byKey["untracked:new file.txt"].Removed)
	binary := byKey["untracked:image.bin"]
	require.True(t, binary.Binary)
	require.Equal(t, -1, binary.Added, "binary counts are explicitly unavailable")
	require.Equal(t, -1, binary.Removed)

	// Git's rename numstat record has an empty path field followed by two NUL paths.
	parsed := parseNumstat([]byte("0\t0\t\x00old name.txt\x00new name.txt\x00"))
	require.Equal(t, numstatCount{added: 0, removed: 0}, parsed["new name.txt"])
}

func TestLocalWorktreeCountsBecomeUnavailableWhenUntrackedBudgetExceeded(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	large := make([]byte, MaxCountFileBytes+1)
	for i := range large {
		large[i] = 'x'
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "too-large.txt"), large, 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	loaded, err := local.LoadWorktree(t.Context(), snapshot.Repositories[0].Worktrees[0].ID)
	require.NoError(t, err, "status remains useful even when optional line metadata is over its bound")
	require.Equal(t, StateError, loaded.CountsState)
	var largeFile FileChange
	for _, file := range loaded.Files {
		if file.Path == "too-large.txt" {
			largeFile = file
		}
	}
	require.Equal(t, -1, largeFile.Added)
	require.Equal(t, -1, largeFile.Removed)
}

func TestParseNumstatBinaryAndRename(t *testing.T) {
	got := parseNumstat([]byte("-\t-\timage.bin\x000\t3\t\x00old.txt\x00new.txt\x002\t1\tname with\ttab.txt\x00"))
	require.Equal(t, numstatCount{added: -1, removed: -1, binary: true}, got["image.bin"])
	require.Equal(t, numstatCount{added: 0, removed: 3}, got["new.txt"])
	require.Equal(t, numstatCount{added: 2, removed: 1}, got["name with\ttab.txt"])
}

func TestLocalWorktreeNumstatMapsRenameToNewPath(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	old := filepath.Join(root, "tracked.txt")
	// A sufficiently similar file makes Git report a staged rename.
	contents := strings.Repeat("same line\n", 20) + "old tail\n"
	require.NoError(t, os.WriteFile(old, []byte(contents), 0o600))
	gitTest(t, root, "add", "tracked.txt")
	gitTest(t, root, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "second")
	gitTest(t, root, "mv", "tracked.txt", "renamed file.txt")
	renamed := filepath.Join(root, "renamed file.txt")
	require.NoError(t, os.WriteFile(renamed, []byte(strings.Repeat("same line\n", 20)+"new tail\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	loaded, err := local.LoadWorktree(t.Context(), snapshot.Repositories[0].Worktrees[0].ID)
	require.NoError(t, err)
	require.Equal(t, StateReady, loaded.CountsState)
	var stagedRename, unstagedRename FileChange
	for _, file := range loaded.Files {
		if file.Path == "renamed file.txt" && file.Group == GroupStaged {
			stagedRename = file
		}
		if file.Path == "renamed file.txt" && file.Group == GroupUnstaged {
			unstagedRename = file
		}
	}
	require.Equal(t, "tracked.txt", stagedRename.OldPath)
	require.Equal(t, 0, stagedRename.Added)
	require.Equal(t, 0, stagedRename.Removed)
	require.Equal(t, 1, unstagedRename.Added)
	require.Equal(t, 1, unstagedRename.Removed)
}

func TestCountUntrackedCountsSymlinkTargetWithoutFollowingIt(t *testing.T) {
	rootPath := t.TempDir()
	link := filepath.Join(rootPath, "link.txt")
	require.NoError(t, os.Symlink("outside\ncontent", link))
	root, err := os.OpenRoot(rootPath)
	require.NoError(t, err)
	defer root.Close()
	lines, binary, bytesRead, err := countUntracked(t.Context(), root, "link.txt", MaxCountBytes, "unspecified")
	require.NoError(t, err)
	assert.Equal(t, 2, lines, "symlink target text uses newline accounting without reading through the link")
	assert.False(t, binary)
	assert.Equal(t, len("outside\ncontent"), bytesRead)
}

func TestLocalUntrackedMetadataHasFileOperationBound(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	for i := range MaxCountFiles + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("empty-%04d", i)), nil, 0o600))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	loaded, err := local.LoadWorktree(t.Context(), snapshot.Repositories[0].Worktrees[0].ID)
	require.NoError(t, err)
	require.Equal(t, StateError, loaded.CountsState, "status remains useful when count metadata exceeds its file-operation bound")
	require.Len(t, loaded.Files, MaxCountFiles+1)
}

func TestCountUntrackedHonorsContext(t *testing.T) {
	rootPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, "file.txt"), []byte("line\n"), 0o600))
	root, err := os.OpenRoot(rootPath)
	require.NoError(t, err)
	defer root.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	lines, binary, bytesRead, err := countUntracked(ctx, root, "file.txt", MaxCountBytes, "unspecified")
	require.Zero(t, lines)
	require.False(t, binary)
	require.Zero(t, bytesRead)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalUntrackedBinaryClassificationHonorsDiffAttributesAndGitHeuristic(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.forcedbin -diff\n*.forcedtext diff\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "forced.forcedbin"), []byte("plain text\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "text.forcedtext"), []byte{'a', 0, 'b', '\n'}, 0o600))
	beforeNUL := append(bytes.Repeat([]byte{'x'}, 7_999), 0)
	beforeNUL = append(beforeNUL, []byte("tail\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(root, "early-nul.dat"), beforeNUL, 0o600))
	afterNUL := append(bytes.Repeat([]byte{'x'}, 8_000), 0)
	afterNUL = append(afterNUL, []byte("tail\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(root, "late-nul.dat"), afterNUL, 0o600))

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	loaded, err := local.LoadWorktree(t.Context(), snapshot.Repositories[0].Worktrees[0].ID)
	require.NoError(t, err)
	require.Equal(t, StateReady, loaded.CountsState)
	byPath := make(map[string]FileChange, len(loaded.Files))
	for _, file := range loaded.Files {
		byPath[file.Path] = file
	}
	require.True(t, byPath["forced.forcedbin"].Binary, "-diff forces binary even when contents contain no NUL")
	require.Equal(t, -1, byPath["forced.forcedbin"].Added)
	require.False(t, byPath["text.forcedtext"].Binary, "diff forces text even when contents contain NUL")
	require.Equal(t, 1, byPath["text.forcedtext"].Added)
	require.True(t, byPath["early-nul.dat"].Binary, "default Git heuristic checks NUL in first 8000 bytes")
	require.False(t, byPath["late-nul.dat"].Binary, "NUL after Git's heuristic prefix does not force binary")
}

func TestUntrackedDiffAttributesReturnUnknownForNamedDriver(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("*.ps diff=postscript\n"), 0o600))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	ref, ok := local.worktree(snapshot.Repositories[0].Worktrees[0].ID)
	require.True(t, ok)
	attrs, err := untrackedDiffAttributes(t.Context(), ref.root, []string{"figure.ps"}, local)
	require.NoError(t, err)
	require.Equal(t, "unknown", attrs["figure.ps"], "driver config may set diff.<driver>.binary=true")
}

func TestLocalDiscoveryReportsActualDepthLimitAndContinuesWithinBound(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	deep := repo
	for i := range DefaultDiscoveryDepth + 1 {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", i))
		require.NoError(t, os.Mkdir(deep, 0o755))
	}
	initRepo(t, filepath.Join(deep, "more", "nested"))

	local, err := NewLocal(repo)
	require.NoError(t, err)
	first, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, DiscoveryLimitDepth, first.Scope.LimitReason)
	require.Equal(t, DefaultDiscoveryDepth, first.Scope.LimitValue)
	require.Contains(t, first.Scope.Note, fmt.Sprintf("depth limit reached (%d)", DefaultDiscoveryDepth))
	require.True(t, local.CanContinueDiscovery())
	require.Len(t, first.Repositories, 1)

	continued, err := local.ContinueDiscovery(t.Context())
	require.NoError(t, err)
	require.Greater(t, continued.Generation, first.Generation)
	require.Equal(t, DiscoveryLimitDepth, continued.Scope.LimitReason)
	require.Equal(t, DefaultDiscoveryDepth+2, continued.Scope.LimitValue)
	require.True(t, local.CanContinueDiscovery())

	continued, err = local.ContinueDiscovery(t.Context())
	require.NoError(t, err)
	require.Empty(t, continued.Scope.LimitReason)
	require.False(t, local.CanContinueDiscovery())
	require.Len(t, continued.Repositories, 2, "a later bounded pass discovers the nested checkout")
	for _, repo := range continued.Repositories {
		for _, wt := range repo.Worktrees {
			require.Equal(t, "unscanned", wt.State, "continuation inventories only and never reads checkout status")
		}
	}
}

func TestLocalDiscoveryPrunesHiddenAndBuildDirectories(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	initRepo(t, filepath.Join(root, ".hidden", "hidden-repo"))
	initRepo(t, filepath.Join(root, "agents-hidden", "hidden-repo"))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".agentsignore"), []byte("agents-hidden/\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cagent-hidden"), 0o755))
	initRepo(t, filepath.Join(root, "cagent-hidden", "hidden-repo"))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".cagentignore"), []byte("cagent-hidden/\n"), 0o600))
	initRepo(t, filepath.Join(root, "node_modules", "dependency"))
	initRepo(t, filepath.Join(root, "visible", "nested-repo"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 2)
	require.Empty(t, snapshot.Scope.LimitReason)
	for _, repository := range snapshot.Repositories {
		require.NotContains(t, repository.Name, "hidden")
		require.NotContains(t, repository.Name, "dependency")
		require.NotContains(t, repository.Name, "agents-hidden")
		require.NotContains(t, repository.Name, "cagent-hidden")
	}
}

func TestLocalDiscoveryEntryBoundIsPartialAndFast(t *testing.T) {
	root := t.TempDir()
	for i := range DefaultDiscoveryEntries + 1 {
		require.NoError(t, os.Mkdir(filepath.Join(root, fmt.Sprintf("dir-%05d", i)), 0o755))
	}
	local, err := NewLocal(root)
	require.NoError(t, err)
	started := time.Now()
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Less(t, time.Since(started), 5*time.Second)
	require.Equal(t, DiscoveryLimitEntries, snapshot.Scope.LimitReason)
	require.Equal(t, DefaultDiscoveryEntries, snapshot.Scope.LimitValue)
	require.Contains(t, snapshot.Scope.Note, "entry limit reached")
}

func TestLocalDiscoveryHonorsCanceledContext(t *testing.T) {
	root := t.TempDir()
	local, err := NewLocal(root)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = local.Snapshot(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalDiscoveryFindsWorktreeRootsAndDoesNotEnterTheirContents(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	worktree := filepath.Join(root, "worktree")
	gitTest(t, root, "worktree", "add", "--quiet", "-b", "linked", worktree)
	initRepo(t, filepath.Join(worktree, "nested"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Repositories, 1, "the linked checkout is inventoried from Git's worktree metadata; its contents are not traversed")
	var linked bool
	for _, repository := range snapshot.Repositories {
		for _, wt := range repository.Worktrees {
			if wt.Linked {
				linked = true
			}
		}
	}
	require.True(t, linked)
}

func TestLocalDiscoveryLargeRepositoryDoesNotCountOrdinaryFilesAsWork(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	files := filepath.Join(root, "many-files")
	require.NoError(t, os.Mkdir(files, 0o755))
	for i := range DefaultDiscoveryEntries + 100 {
		require.NoError(t, os.WriteFile(filepath.Join(files, fmt.Sprintf("file-%05d", i)), nil, 0o600))
	}
	gitTest(t, root, "add", "many-files")
	gitTest(t, root, "commit", "--quiet", "-m", "many tracked files")
	local, err := NewLocal(root)
	require.NoError(t, err)
	started := time.Now()
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Less(t, time.Since(started), 5*time.Second)
	require.Empty(t, snapshot.Scope.LimitReason, "ordinary file content does not consume nested-repository traversal budget")
	require.Len(t, snapshot.Repositories, 1)
}

func TestLocalDiscoveryFindsRepositoryAtDefaultDepthBoundary(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	path := root
	for i := 1; i < DefaultDiscoveryDepth; i++ {
		path = filepath.Join(path, fmt.Sprintf("level-%d", i))
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	nearby := filepath.Join(path, "nearby-repo")
	initRepo(t, nearby)

	local, err := NewLocal(root)
	require.NoError(t, err)
	snapshot, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Scope.LimitReason)
	var found bool
	for _, repo := range snapshot.Repositories {
		found = found || repo.Name == "nearby-repo"
	}
	require.True(t, found, "repositories at the configured shallow depth boundary are included")
}
