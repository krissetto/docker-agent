package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type pendingEditHandle struct {
	*lifecycleHandle

	edits   chan runtime.SessionEdit
	failure error
}

func (h *pendingEditHandle) Edit(_ context.Context, edit runtime.SessionEdit) (*session.Session, error) {
	h.edits <- edit
	return nil, h.failure
}

type pendingEditSessions struct {
	*openSubagentSessions

	handle *pendingEditHandle
}

func (s *pendingEditSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func TestActualProgramQueuedEditRetainsIdentityOrderAndFailureDraft(t *testing.T) {
	for _, failure := range []string{"", "message no longer pending", "queued message changed since editing began"} {
		fail := failure != ""
		name := "success"
		if fail {
			name = failure
		}
		t.Run(name, func(t *testing.T) {
			sess := session.New(session.WithID("pending-edit-owner"), session.WithTitle("PENDING-EDITOR"))
			handle := &pendingEditHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, edits: make(chan runtime.SessionEdit, 1)}
			if fail {
				handle.failure = errors.New(failure)
			}
			application := app.New(t.Context(), &pendingEditSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			root := newSidebarProgramRoot(t, application)
			root.editor.SetValue("COMPOSER-UNCHANGED")
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			program.Send(runtime.PendingUserMessageAccepted(sess.ID, "first-turn-ID", "First untouched", nil, 1))
			program.Send(runtime.PendingUserMessageAccepted(sess.ID, "second-turn-ID", "Original full text", nil, 2))
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && strings.Contains(ansi.Strip(s.content), "Original full text")
			}, time.Second, time.Millisecond)
			frame := sidebarProgramSnapshot(t, program)
			x, y := sidebarProgramPoint(t, frame.content, "Original full text")
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			program.Send(tea.PasteMsg{Content: " EDITED"})
			program.Send(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
			var edit runtime.SessionEdit
			select {
			case edit = <-handle.edits:
			case <-time.After(time.Second):
				t.Fatal("canonical edit not called")
			}
			require.Equal(t, runtime.SessionEditPendingMessage, edit.Kind)
			require.Equal(t, "second-turn-ID", edit.PendingMessage.TurnID)
			require.NotNil(t, edit.PendingMessage.ExpectedContent)
			require.Equal(t, "Original full text", *edit.PendingMessage.ExpectedContent)
			require.Contains(t, edit.PendingMessage.Content, "EDITED")
			if fail {
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.open && strings.Contains(ansi.Strip(s.content), failure)
				}, time.Second, time.Millisecond)
				require.Contains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "EDITED", "failure keeps editor draft")
				program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			} else {
				program.Send(runtime.PendingUserMessageEdited(sess.ID, "second-turn-ID", edit.PendingMessage.Content, nil, 2))
			}
			require.Eventually(t, func() bool { return !sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			final := sidebarProgramSnapshot(t, program)
			require.Equal(t, "COMPOSER-UNCHANGED", final.editor)
			require.Contains(t, ansi.Strip(final.content), "First untouched")
			if !fail {
				require.Contains(t, ansi.Strip(final.content), "EDITED")
			}
			require.NotContains(t, ansi.Strip(final.content), "Failed to remove")
			program.Send(messages.ShowInteractionHintMsg{SessionID: sess.ID})
		})
	}
}

func TestPendingEditorSaveRejectsChangedSessionBeforeWorker(t *testing.T) {
	sess := session.New(session.WithID("editor-origin"), session.WithTitle("ORIGIN"))
	handle := &pendingEditHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, edits: make(chan runtime.SessionEdit, 1)}
	application := app.New(t.Context(), &pendingEditSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	_, _ = root.Update(messages.OpenPendingEditMsg{SessionID: sess.ID, TurnID: "exact-pending-turn", Content: "PRESERVED-DRAFT"})
	require.True(t, root.dialogMgr.Open())
	replacement := session.New(session.WithID("different-owner"))
	root.application = app.New(t.Context(), nil, replacement, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	for _, msg := range collectMsgs(cmd) {
		root.Update(msg)
	}
	require.True(t, root.dialogMgr.Open())
	require.Contains(t, ansi.Strip(root.dialogMgr.TopDialog().View()), "session changed")
	require.Contains(t, ansi.Strip(root.dialogMgr.TopDialog().View()), "PRESERVED-DRAFT")
	select {
	case <-handle.edits:
		t.Fatal("changed owner must not reach backend save")
	default:
	}
}
