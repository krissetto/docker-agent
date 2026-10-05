package builtins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestLargeResultOwnershipKeysAreInert(t *testing.T) {
	// External IDs are only map keys; these checks do not execute any filesystem sink.
	for _, id := range []string{".", "..", "a/b", "%2e%2e", "a%2fb"} {
		require.Equal(t, id, largeResultKey(t.Context(), id).sessionID)
		owner := tools.NewResourceOwner()
		require.Equal(t, largeToolResultKey{owner: owner}, largeResultKey(tools.WithResourceOwner(t.Context(), owner), id))
	}
}

func TestLargeResultCleanupOnlyAllocatedOwnerDirectory(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)
	sentinel := filepath.Join(parent, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o600))
	l := newLargeToolResultLimiter()
	ctxA := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	ctxB := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	end := &hooks.Input{SessionID: "shared-public-id", HookEventName: hooks.EventSessionEnd}
	_, err := l.dispatch(ctxA, end, nil)
	require.NoError(t, err)
	require.Empty(t, l.dirs)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no allocation or removal before spill")
	spill := &hooks.Input{SessionID: end.SessionID, HookEventName: hooks.EventToolResponseTransform, ToolCategory: "shell", ToolResponse: strings.Repeat("x", maxToolCallResultBytes+1)}
	for _, ctx := range []context.Context{ctxA, ctxB} {
		_, err = l.dispatch(ctx, spill, nil)
		require.NoError(t, err)
	}
	dirA := l.dirs[largeResultKey(ctxA, end.SessionID)]
	dirB := l.dirs[largeResultKey(ctxB, end.SessionID)]
	require.NotEqual(t, dirA, dirB)
	require.Equal(t, parent, filepath.Dir(dirA))
	require.Equal(t, parent, filepath.Dir(dirB))
	otherRegistry := newLargeToolResultLimiter()
	_, err = otherRegistry.dispatch(ctxA, spill, nil)
	require.NoError(t, err)
	dirOther := otherRegistry.dirs[largeResultKey(ctxA, end.SessionID)]
	require.Equal(t, parent, filepath.Dir(dirOther))
	require.NotEqual(t, dirA, dirOther, "registrations do not share allocations")
	_, err = l.dispatch(ctxA, end, nil)
	require.NoError(t, err)
	require.NoDirExists(t, dirA)
	require.DirExists(t, dirOther)
	require.DirExists(t, dirB)
	require.FileExists(t, sentinel)
	_, err = l.dispatch(ctxB, end, nil)
	require.NoError(t, err)
	require.NoDirExists(t, dirB)
	require.FileExists(t, sentinel)
	_, err = otherRegistry.dispatch(ctxA, end, nil)
	require.NoError(t, err)
	require.NoDirExists(t, dirOther)
	require.FileExists(t, sentinel)
}
