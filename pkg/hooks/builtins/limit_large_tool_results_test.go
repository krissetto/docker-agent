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

func TestLimitLargeToolResultsSpillFailure(t *testing.T) {
	parent := t.TempDir()
	blocked := filepath.Join(parent, "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	t.Setenv("TMPDIR", blocked)
	for _, test := range []struct{ category, name, payload string }{
		{"shell", "shell", "discarded head" + strings.Repeat("世", 100_000) + "diagnostic tail"},
		{"filesystem", "read_file", "important beginning" + strings.Repeat("世", 100_000)},
		{"shell", "shell", strings.Repeat("x\n", 2001)},
	} {
		l := newLargeToolResultLimiter()
		out, err := l.dispatch(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolCategory: test.category, ToolName: test.name, ToolResponse: test.payload}, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		got := *out.HookSpecificOutput.UpdatedToolResponse
		require.LessOrEqual(t, len(got), maxToolCallResultBytes)
		require.Equal(t, got, strings.ToValidUTF8(got, ""))
		require.Contains(t, got, "could not be saved")
		require.NotContains(t, got, "available in a file:")
		require.NotEqual(t, test.payload, got)
		require.Empty(t, l.dirs)
		if test.name == "read_file" {
			require.Contains(t, got, "important beginning")
		} else if strings.Contains(test.payload, "diagnostic tail") {
			require.Contains(t, got, "diagnostic tail")
		}
	}
}

func TestLimitLargeToolResultsAllocatedDirectoryFailure(t *testing.T) {
	l := newLargeToolResultLimiter()
	ctx := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	key := largeResultKey(ctx, "shared")
	l.dirs[key] = filepath.Join(t.TempDir(), "removed-directory")
	out, err := l.dispatch(ctx, &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolCategory: "shell", ToolResponse: strings.Repeat("x", 100_000)}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.LessOrEqual(t, len(*out.HookSpecificOutput.UpdatedToolResponse), maxToolCallResultBytes)
	require.Contains(t, *out.HookSpecificOutput.UpdatedToolResponse, "could not be saved")
}
