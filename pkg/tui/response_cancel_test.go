package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type responseHandle struct {
	*lifecycleHandle

	cancels      atomic.Int32
	canceledTurn atomic.Value
}

func (h *responseHandle) Cancel(_ context.Context, turn string) (runtime.CancelResult, error) {
	h.cancels.Add(1)
	h.canceledTurn.Store(turn)
	return runtime.CancelResult{SessionID: h.id, TurnID: turn, Outcome: runtime.CancelAccepted}, nil
}

type responseSessions struct {
	*openSubagentSessions

	handle *responseHandle
}

func (s *responseSessions) SessionByID(string) (runtime.SessionHandle, error) { return s.handle, nil }

func responseTestRoot(t *testing.T) (*appModel, *responseHandle) {
	t.Helper()
	sess := session.New(session.WithID("response-confirm-session"), session.WithAgentName("root"))
	sess.Title = "Response confirmation test"
	handle := &responseHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}}
	application := app.New(t.Context(), &responseSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := application.FollowUpMessage(t.Context(), "active turn", nil)
	require.NoError(t, err)
	root := newSidebarProgramRoot(t, application)
	root.editor.Blur()
	root.focusedPanel = PanelContent
	return root, handle
}

func waitForResponseAnimation(t *testing.T, model *sidebarHoverProgram) {
	t.Helper()
	require.Eventually(t, func() bool { frames := model.snapshot(); return len(frames) > 0 && frames[len(frames)-1].ticks > 0 }, time.Second, time.Millisecond, "StreamStarted must propagate its original shared-clock command before Escape")
}

func TestActualProgramResponseDoubleEscapeCancelsOnceWithoutDialog(t *testing.T) {
	for _, mode := range []messages.InterruptMode{messages.InterruptModeAlways, messages.InterruptModeDoubleTap, messages.InterruptModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			root, handle := responseTestRoot(t)
			root.interruptMode = mode
			root.chatPage.SetInterruptMode(mode)
			writer := &cacheProgramWriter{}
			model := &sidebarHoverProgram{root: root}
			program := startTestProgram(t, root, model, tea.WithOutput(writer))
			program.Send(runtime.StreamStarted("response-confirm-session", "root"))
			waitForResponseAnimation(t, model)
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape, IsRepeat: true})
			require.Zero(t, handle.cancels.Load())
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool {
				f := model.snapshot()
				return len(f) > 0 && strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again to cancel the response.")
			}, time.Second, time.Millisecond)
			require.Zero(t, handle.cancels.Load(), "first Escape never calls cancellation")
			frames := model.snapshot()
			require.NotContains(t, ansi.Strip(frames[len(frames)-1].content), "Interrupt response")
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape, IsRepeat: true})
			require.Zero(t, handle.cancels.Load(), "autorepeat cannot accept prompt")
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Eventually(t, func() bool { return handle.cancels.Load() == 1 }, time.Second, time.Millisecond)
			require.Equal(t, "accepted", handle.canceledTurn.Load())
			require.Eventually(t, func() bool {
				f := model.snapshot()
				return strings.Contains(ansi.Strip(f[len(f)-1].content), "Response cancelled.")
			}, time.Second, time.Millisecond)
			program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Equal(t, int32(1), handle.cancels.Load(), "completed cancellation cannot dispatch twice")
			require.Eventually(t, func() bool {
				f := model.snapshot()
				last := f[len(f)-1]
				return last.active == 0 && !strings.Contains(ansi.Strip(last.content), "Response cancelled.")
			}, 2*time.Second, time.Millisecond)
			select {
			case <-time.After(80 * time.Millisecond):
			case <-t.Context().Done():
				t.Fatal("cancelled waiting final flush")
			}
			writes := len(writer.snapshot())
			frames = model.snapshot()
			ticks := frames[len(frames)-1].ticks
			require.Never(t, func() bool {
				f := model.snapshot()
				return f[len(f)-1].ticks != ticks || len(writer.snapshot()) != writes
			}, 80*time.Millisecond, time.Millisecond, "expired success notice owns no idle ticks or writes")
		})
	}
}

func TestResponsePromptDisarmsOnInputFinishRestartAndModal(t *testing.T) {
	root, handle := responseTestRoot(t)
	_, _ = root.Update(runtime.StreamStarted("response-confirm-session", "root"))
	defer root.ar.Stop()
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, root.responsePrompt.armed)
	_, _ = root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.False(t, root.responsePrompt.armed)
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, handle.cancels.Load(), "different input requires a new first Escape")
	_, _ = root.Update(runtime.StreamStopped(root.application.Session().ID, "root", "completed"))
	require.False(t, root.responsePrompt.armed)
	_, _ = root.Update(runtime.StreamStarted(root.application.Session().ID, "root"))
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	_, _ = root.Update(runtime.StreamStarted(root.application.Session().ID, "root"))
	require.False(t, root.responsePrompt.armed, "new run cannot inherit old confirmation")
	_, _ = root.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(help.Document{})})
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, root.responsePrompt.armed, "modal Escape only dismisses its dialog")
	require.Zero(t, handle.cancels.Load())
}

func TestActualProgramResponsePromptExpiresWithoutInput(t *testing.T) {
	root, handle := responseTestRoot(t)
	model := &sidebarHoverProgram{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(runtime.StreamStarted("response-confirm-session", "root"))
	waitForResponseAnimation(t, model)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return len(f) > 0 && strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return !strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, responsePromptLifetime+time.Second, time.Millisecond, "shared deadline expires without an input wakeup")
	require.Zero(t, handle.cancels.Load())
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, time.Second, time.Millisecond)
	require.Zero(t, handle.cancels.Load(), "Escape after expiry is first Escape, not cancellation")
	program.Send(runtime.StreamStopped("response-confirm-session", "root", "completed"))
	require.Eventually(t, func() bool { f := model.snapshot(); return f[len(f)-1].active == 0 }, time.Second, time.Millisecond)
}

func TestResponsePromptCannotCancelAnotherSession(t *testing.T) {
	root, first := responseTestRoot(t)
	_, _ = root.Update(runtime.StreamStarted("response-confirm-session", "root"))
	secondSess := session.New(session.WithID("response-second-session"), session.WithAgentName("root"))
	second := &responseHandle{lifecycleHandle: &lifecycleHandle{id: secondSess.ID}}
	secondApp := app.New(t.Context(), &responseSessions{openSubagentSessions: &openSubagentSessions{}, handle: second}, secondSess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := secondApp.FollowUpMessage(t.Context(), "second active turn", nil)
	require.NoError(t, err)
	_, err = root.supervisor.AddSession(t.Context(), secondApp, secondSess, "", nil)
	require.NoError(t, err)
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, root.responsePrompt.armed)
	root.handleSwitchTab(secondSess.ID)
	require.False(t, root.responsePrompt.armed)
	_, _ = root.Update(runtime.StreamStarted(secondSess.ID, "root"))
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, root.responsePrompt.armed)
	require.Zero(t, first.cancels.Load())
	require.Zero(t, second.cancels.Load(), "first Escape on another tab cannot accept old prompt")
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, first.cancels.Load())
	require.Equal(t, int32(1), second.cancels.Load())
}

func TestActualProgramResponsePromptRunAndModalGuards(t *testing.T) {
	root, handle := responseTestRoot(t)
	model := &sidebarHoverProgram{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(runtime.StreamStarted("response-confirm-session", "root"))
	waitForResponseAnimation(t, model)
	promptVisible := func() bool {
		frames := model.snapshot()
		return len(frames) > 0 && strings.Contains(ansi.Strip(frames[len(frames)-1].content), "Press Esc again")
	}
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, promptVisible, time.Second, time.Millisecond)
	program.Send(runtime.StreamStopped("response-confirm-session", "root", "completed"))
	program.Send(runtime.StreamStarted("response-confirm-session", "root"))
	waitForResponseAnimation(t, model)
	require.Eventually(t, func() bool { return !promptVisible() }, time.Second, time.Millisecond, "new run disarms previous prompt before any new Escape")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, promptVisible, time.Second, time.Millisecond)
	require.Zero(t, handle.cancels.Load(), "new-run Escape cannot cancel by inheriting prior prompt")
	program.Send(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(help.Document{})})
	require.Eventually(t, func() bool { return !promptVisible() }, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, handle.cancels.Load(), "modal Escape dismisses, never arms or cancels underneath")
	// Wait for the existing close lifecycle, then first Escape must freshly arm.
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return !f[len(f)-1].modal
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape, IsRepeat: true})
	require.Zero(t, handle.cancels.Load())
	program.Send(runtime.StreamStopped("response-confirm-session", "root", "completed"))
	require.Eventually(t, func() bool { f := model.snapshot(); return f[len(f)-1].active == 0 }, time.Second, time.Millisecond)
}

func TestActualProgramResponsePromptSwitchCannotCancelWrongSession(t *testing.T) {
	root, first := responseTestRoot(t)
	sess := session.New(session.WithID("response-other-live"), session.WithAgentName("root"))
	sess.Title = "Other response test"
	second := &responseHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}}
	application := app.New(t.Context(), &responseSessions{openSubagentSessions: &openSubagentSessions{}, handle: second}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := application.FollowUpMessage(t.Context(), "other run", nil)
	require.NoError(t, err)
	_, err = root.supervisor.AddSession(t.Context(), application, sess, "", nil)
	require.NoError(t, err)
	model := &sidebarHoverProgram{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(runtime.StreamStarted("response-confirm-session", "root"))
	waitForResponseAnimation(t, model)
	root.supervisor.SetProgram(program)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return len(f) > 0 && strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, time.Second, time.Millisecond)
	program.Send(messages.SwitchTabMsg{SessionID: sess.ID})
	program.Send(messages.RoutedMsg{SessionID: "response-confirm-session", Inner: runtime.StreamStopped("response-confirm-session", "root", "completed")})
	program.Send(runtime.StreamStarted(sess.ID, "root"))
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return !strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return strings.Contains(ansi.Strip(f[len(f)-1].content), "Press Esc again")
	}, time.Second, time.Millisecond)
	require.Zero(t, first.cancels.Load())
	require.Zero(t, second.cancels.Load(), "freshly selected run requires its own second Escape")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool { return second.cancels.Load() == 1 }, time.Second, time.Millisecond)
	require.Zero(t, first.cancels.Load())
	require.Equal(t, "accepted", second.canceledTurn.Load())
	require.Eventually(t, func() bool { f := model.snapshot(); return f[len(f)-1].active == 0 }, 2*time.Second, time.Millisecond)
}

func TestResponsePromptRejectsExpiredSecondEscapeBeforeTickDelivery(t *testing.T) {
	root, handle := responseTestRoot(t)
	_, _ = root.Update(runtime.StreamStarted("response-confirm-session", "root"))
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.True(t, root.responsePrompt.deadline.Running())
	root.responsePrompt.expiresAt = time.Now().Add(-time.Nanosecond)
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Zero(t, handle.cancels.Load(), "a delayed animation tick cannot extend the confirmation window")
	require.True(t, root.responsePrompt.armed, "expired second Escape starts a fresh prompt")
	require.True(t, time.Now().Before(root.responsePrompt.expiresAt))
}
