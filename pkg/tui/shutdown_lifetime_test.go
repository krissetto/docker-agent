package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/gitbranch"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
)

// cleanupCountingEditor records Cleanup calls and forwards them to the real editor.
type cleanupCountingEditor struct {
	editor.Editor
	cleanups *int
}

func (e cleanupCountingEditor) Cleanup() {
	*e.cleanups++
	e.Editor.Cleanup()
}

// TestShutdownReleasesChatPageAndEditorLifetimes pins the harness shutdown
// path: Shutdown must cancel each chat page's context, which owns the
// sidebar's git branch watcher, and clean up every editor. Otherwise each
// stopped TUI keeps its watcher's inotify instance and goroutine alive for
// the rest of the process.
func TestShutdownReleasesChatPageAndEditorLifetimes(t *testing.T) {
	started := make(chan context.Context, 16)
	restore := sidebar.SetBranchWatcherForTesting(func(ctx context.Context, dir string) (*gitbranch.Watcher, error) {
		select {
		case started <- ctx:
		default:
		}
		return gitbranch.Watch(ctx, dir)
	})
	t.Cleanup(restore)

	root, _, _ := frozenClockRoot(t, 120, 40)
	var branches []context.Context
	for drained := false; !drained; {
		select {
		case ctx := <-started:
			branches = append(branches, ctx)
		default:
			drained = true
		}
	}
	require.NotEmpty(t, branches, "building the TUI must start the sidebar's branch watcher")
	alive := 0
	for _, ctx := range branches {
		if ctx.Err() == nil {
			alive++
		}
	}
	require.Equal(t, len(root.chatPages), alive, "exactly the live chat page's branch watcher must run while the TUI is alive")

	cleanups := 0
	for id, ed := range root.editors {
		root.editors[id] = cleanupCountingEditor{Editor: ed, cleanups: &cleanups}
	}
	require.NotEmpty(t, root.editors)

	root.Shutdown()
	for _, ctx := range branches {
		require.Error(t, ctx.Err(), "Shutdown must cancel every chat page context and its branch watcher")
	}
	require.Equal(t, len(root.editors), cleanups, "Shutdown must clean up every editor")

	// The harness cleanup and an explicit Shutdown may both run; a second
	// call must be harmless.
	require.NotPanics(t, root.Shutdown)
}
