package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type blockingSessionSummaryStore struct {
	session.Store
	entered chan struct{}
	done    chan struct{}
}

func (s *blockingSessionSummaryStore) GetSessionSummaries(ctx context.Context) ([]session.Summary, error) {
	defer close(s.done)
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

type sessionBrowserServices struct {
	compactTestServices
	store session.Store
}

func (s sessionBrowserServices) SessionStore() session.Store { return s.store }

func TestOpenSessionBrowserReturnsBeforeStorageAndShowsLoadingDialog(t *testing.T) {
	store := &blockingSessionSummaryStore{
		Store:   session.NewInMemorySessionStore(),
		entered: make(chan struct{}),
		done:    make(chan struct{}),
	}

	model, _ := newTestModel(t)
	model.application = app.New(t.Context(), nil, session.New(), runtime.SessionBinding{}, app.WithRuntimeServices(sessionBrowserServices{store: store}))
	model.supervisor = supervisor.New(nil)
	model.dialogMgr = dialog.New(model.ar)
	model.dialogMgr.SetSize(100, 30)
	t.Cleanup(model.dialogMgr.Cleanup)

	_, openCmd := model.handleOpenSessionBrowser()
	require.NotNil(t, openCmd)
	open, ok := openCmd().(dialog.OpenDialogMsg)
	require.True(t, ok)
	require.NotNil(t, open.Model)
	select {
	case <-store.entered:
		t.Fatal("session storage was read before the dialog opened")
	default:
	}

	open.Model.SetSize(100, 30)
	require.Contains(t, open.Model.View(), "Loading sessions…")
	managerCmd := model.updateDialogCmd(open)
	require.Same(t, open.Model, model.dialogMgr.TopDialog())
	launchDialogCommands(managerCmd)

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("session summary load did not start")
	}
	view := open.Model.View()
	require.Contains(t, view, "Loading sessions…", "the modal remains usable while the store is blocked")
	require.Same(t, open.Model, model.dialogMgr.TopDialog())

	select {
	case <-store.done:
		t.Fatal("storage unexpectedly returned before cancellation")
	default:
	}
	model.dialogMgr.Cleanup()
	select {
	case <-store.done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked store did not finish after dialog cancellation")
	}
}

func launchDialogCommands(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, child := range batch {
				launchDialogCommands(child)
			}
		}
	}()
}
