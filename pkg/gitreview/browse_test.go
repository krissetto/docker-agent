package gitreview

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalBrowseRootInventoriesOnlyScopeRootWithoutNearbyDiscovery(t *testing.T) {
	root := t.TempDir()
	for i := range 30 {
		initEmptyCheckout(t, filepath.Join(root, fmt.Sprintf("repo-%03d", i)))
	}
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	trace := filepath.Join(t.TempDir(), "trace")
	binDir := t.TempDir()
	shim := "#!/bin/sh\nprintf x >> \"$GIT_REVIEW_DISCOVERY_TRACE\"\nexec \"$REAL_GIT\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("GIT_REVIEW_DISCOVERY_TRACE", trace)

	local, err := NewLocal(root)
	require.NoError(t, err)
	browse, err := local.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.True(t, browse.Browse)
	require.NotZero(t, browse.Generation)
	require.Len(t, browse.Repositories, 1, "only the synthetic workspace root is inventoried")
	require.Empty(t, browse.Scope.LimitReason)
	_, err = os.Stat(trace)
	require.ErrorIs(t, err, os.ErrNotExist, "browse-only opening never spawns Git discovery commands")

	directory, err := local.Directory(t.Context(), browse.Repositories[0].Worktrees[0].ID, ".")
	require.NoError(t, err)
	require.Len(t, directory.Entries, 30, "the root folder lists lazily without a nearby walk")

	full, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.False(t, full.Browse)
	require.Greater(t, full.Generation, browse.Generation)
	require.Len(t, full.Repositories, 31)
}

func TestLocalBrowseRootKeepsExistingInventory(t *testing.T) {
	root := t.TempDir()
	initEmptyCheckout(t, filepath.Join(root, "nearby"))
	local, err := NewLocal(root)
	require.NoError(t, err)
	full, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	browse, err := local.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.False(t, browse.Browse, "an initialized inventory that already has the root is not replaced")
	require.Equal(t, full.Generation, browse.Generation)
	require.Len(t, browse.Repositories, len(full.Repositories))
}

func TestLocalBrowseRootUsesScopeCheckoutIdentityForSubdirectory(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	scope := filepath.Join(root, "allowed")
	require.NoError(t, os.MkdirAll(filepath.Join(scope, "inner"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(scope, "file.txt"), []byte("x"), 0o600))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	browse, err := local.BrowseRoot(t.Context())
	require.NoError(t, err)
	require.Len(t, browse.Repositories, 1)
	wt := browse.Repositories[0].Worktrees[0]
	require.True(t, wt.Current)
	full, err := local.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, full.Repositories[0].Worktrees[0].ID, wt.ID, "the root checkout keeps the same ID across browse and full inventories")
	directory, err := local.Directory(t.Context(), wt.ID, ".")
	require.NoError(t, err)
	require.Len(t, directory.Entries, 2, "subdirectory browsing is relative to the scope, not the checkout root")
}

func TestLocalBrowseRootDoesNotReadNestedSubtrees(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied fixtures need a non-root user")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	initEmptyCheckout(t, filepath.Join(locked, "repo"))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	local, err := NewLocal(root)
	require.NoError(t, err)
	browse, err := local.BrowseRoot(t.Context())
	require.NoError(t, err, "an unreadable nested subtree is never opened by browse-only inventory")
	require.True(t, browse.Browse)
	require.Len(t, browse.Repositories, 1)
}

func TestLocalBrowseRootDirectoryContainsEscapesBeforeFullSnapshot(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "scope")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(scope, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(scope, "link")))
	local, err := NewLocal(scope)
	require.NoError(t, err)
	browse, err := local.BrowseRoot(t.Context())
	require.NoError(t, err)
	id := browse.Repositories[0].Worktrees[0].ID

	_, err = local.Directory(t.Context(), id, "../outside")
	require.ErrorIs(t, err, ErrOutsideScope)
	_, err = local.Directory(t.Context(), id, "link")
	require.ErrorIs(t, err, ErrOutsideScope, "a symlink that leaves the scope is not browsable")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = local.Directory(ctx, id, ".")
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalBrowseRootCanceledDoesNotPublishInventory(t *testing.T) {
	local, err := NewLocal(t.TempDir())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = local.BrowseRoot(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, local.CurrentSnapshot().Generation, "a canceled browse leaves no inventory behind")
}
