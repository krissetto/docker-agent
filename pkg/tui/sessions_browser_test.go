package tui

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/sessionbrowser"
)

type browserPagerFixture struct {
	*lifecycleSessions
	calls []runtime.SessionSummaryPageOptions
	page  runtime.SessionSummaryPage
}

func (r *browserPagerFixture) ListSessionSummaryPage(_ context.Context, options runtime.SessionSummaryPageOptions) (runtime.SessionSummaryPage, error) {
	r.calls = append(r.calls, options)
	return r.page, nil
}

func TestSessionsBrowserBoundedSearchGenerationAndProjectGrouping(t *testing.T) {
	root := splitTestRoot(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.workspaceUI.visible = true
	dir := t.TempDir()
	rt := &browserPagerFixture{lifecycleSessions: newLifecycleSessions(), page: runtime.SessionSummaryPage{Entries: []runtime.SessionSummaryEntry{{SessionID: "stored", Title: "Generated search title", WorkingDir: dir}}, NextCursor: "opaque-next"}}
	a := app.New(t.Context(), rt, root.application.Session(), runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root.application = a
	root.supervisor.GetRunner("profile").App = a
	cmd := root.requestSessionPage(false)
	require.Empty(t, rt.calls, "catalog I/O runs off owner loop")
	stale := cmd().(sessionPageResult)
	root.debounceSessionQuery("  title  ")
	root.acceptSessionPage(stale)
	require.Empty(t, root.workspaceUI.rows, "old query cannot repopulate result rows")
	result := root.requestSessionPage(false)().(sessionPageResult)
	root.acceptSessionPage(result)
	require.Equal(t, "title", rt.calls[1].Query)
	require.Equal(t, 50, rt.calls[1].Limit)
	require.True(t, rt.calls[1].IncludeChildren)
	require.Len(t, root.workspaceUI.rows, 1)
	normalized, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, normalized, root.workspaceUI.rows[0].ProjectID)
	next := root.requestSessionPage(true)
	require.NotNil(t, next)
	next()
	require.Equal(t, "opaque-next", rt.calls[2].Cursor)
}

func TestWorkspaceAsyncOpenFencedAndPreservesCanonicalOwners(t *testing.T) {
	root, rt := paneSourceFixture(t)
	root.workspaceUI.browser = sessionbrowser.New()
	root.savePaneWorkspace(root.paneFocus())
	original := root.paneWorkspaces.active
	page, editor := root.chatPage, root.editor
	cmd := root.openWorkspaceSession("closed-source", "split")
	require.Zero(t, rt.prepares.Load())
	result := cmd().(workspaceOpenedMsg)
	root.createWorkspace()
	root.finishWorkspaceOpen(result)
	require.Nil(t, root.supervisor.FindBySession("closed-source"), "late acquired view does not land in another workspace")
	require.Same(t, page, root.chatPages["profile"])
	require.Same(t, editor, root.editors["profile"])
	root.activateWorkspace(original.id)
	result = root.openWorkspaceSession("closed-source", "split")().(workspaceOpenedMsg)
	root.finishWorkspaceOpen(result)
	require.NotNil(t, root.supervisor.FindBySession("closed-source"))
	require.Len(t, root.panes.Sessions(), 2)
	require.Equal(t, "original draft", root.editors["profile"].Value())
}
