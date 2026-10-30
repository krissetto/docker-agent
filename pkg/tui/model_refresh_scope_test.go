package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// refreshCatalogHandle blocks catalog refresh until the test releases it.
type refreshCatalogHandle struct {
	*sidebarModelHandle

	started chan struct{}
	release chan struct{}
	err     error
}

func (h *refreshCatalogHandle) Metadata() runtime.SessionMetadata {
	md := h.sidebarModelHandle.Metadata()
	md.Capabilities.ModelCatalogRefresh = true
	return md
}

func (h *refreshCatalogHandle) RefreshModelsCatalog(context.Context) error {
	close(h.started)
	<-h.release
	return h.err
}

type refreshCatalogSessions struct {
	*openSubagentSessions

	handle *refreshCatalogHandle
}

func (s *refreshCatalogSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func newRefreshCatalogRoot(t *testing.T, err error) (*appModel, *refreshCatalogHandle) {
	t.Helper()
	sess := session.New(session.WithID("refresh-session"), session.WithAgentName("worker"), session.WithWorkingDir("/workspace/refresh"))
	handle := &refreshCatalogHandle{
		sidebarModelHandle: &sidebarModelHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, id: sess.ID},
		started:            make(chan struct{}),
		release:            make(chan struct{}),
		err:                err,
	}
	application := app.New(t.Context(), &refreshCatalogSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(stubRuntime{}))
	require.Same(t, handle, application.SessionHandle())
	return newSidebarProgramRoot(t, application), handle
}

// switchToOtherTab adds a second session and activates it, leaving the refresh origin.
func switchToOtherTab(t *testing.T, root *appModel) {
	t.Helper()
	sess := session.New(session.WithID("other-session"), session.WithWorkingDir("/workspace/other"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), a, sess, "", nil)
	require.NoError(t, err)
	page, state, ed, application := root.chatPage, root.sessionState, root.editor, root.application
	root.initSessionComponents(sess.ID, a, sess)
	root.chatPage, root.sessionState, root.editor, root.application = page, state, ed, application
	_, _ = root.handleSwitchTab(sess.ID)
	require.Equal(t, sess.ID, root.supervisor.ActiveID())
}

// runRefresh starts the refresh command and returns a channel for its result.
func runRefresh(t *testing.T, root *appModel, handle *refreshCatalogHandle) <-chan tea.Msg {
	t.Helper()
	_, cmd := root.handleRefreshModelPicker("q")
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	done := make(chan tea.Msg, 1)
	go func() { done <- batch[1]() }()
	<-handle.started
	return done
}

func TestModelRefreshDeliversDialogOnOriginatingTab(t *testing.T) {
	root, handle := newRefreshCatalogRoot(t, nil)
	done := runRefresh(t, root, handle)
	close(handle.release)
	result := <-done
	_, effects := root.Update(result)
	for _, msg := range collectMsgs(effects) {
		_, _ = root.Update(msg)
	}
	assert.True(t, root.dialogMgr.Open(), "a refresh completing on its own tab opens the model picker")
}

func TestModelRefreshDropsResultAfterTabSwitch(t *testing.T) {
	root, handle := newRefreshCatalogRoot(t, nil)
	done := runRefresh(t, root, handle)
	switchToOtherTab(t, root)
	close(handle.release)
	result := <-done
	_, effects := root.Update(result)
	assert.Empty(t, refreshOutcomes(collectMsgs(effects)), "a refresh that completes after its tab was left must not open a dialog or toast")
	assert.False(t, root.dialogMgr.Open())
}

func TestModelRefreshErrorDropsAfterTabSwitch(t *testing.T) {
	root, handle := newRefreshCatalogRoot(t, errors.New("discovery failed"))
	done := runRefresh(t, root, handle)
	switchToOtherTab(t, root)
	close(handle.release)
	result := <-done
	_, effects := root.Update(result)
	assert.Empty(t, refreshOutcomes(collectMsgs(effects)), "a failed refresh completing after its tab was left reports nothing")
}

// refreshOutcomes keeps only the toasts and dialogs a refresh result can produce.
func refreshOutcomes(msgs []tea.Msg) []tea.Msg {
	var out []tea.Msg
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case dialog.OpenDialogMsg:
			out = append(out, msg)
		case notification.ShowMsg:
			if strings.Contains(msg.Text, "efresh") || strings.Contains(msg.Text, "reloaded") || strings.Contains(msg.Text, "No models") {
				out = append(out, msg)
			}
		}
	}
	return out
}
