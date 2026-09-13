package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type confirmationHandle struct {
	*lifecycleHandle

	responses chan runtime.InteractionResponse
}

func (h *confirmationHandle) Respond(_ context.Context, response runtime.InteractionResponse) error {
	h.responses <- response
	return nil
}

type confirmationSessions struct {
	*openSubagentSessions

	handle *confirmationHandle
}

func (s *confirmationSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func startToolConfirmationProgram(t *testing.T) (*tea.Program, *confirmationHandle) {
	t.Helper()
	sess := session.New(session.WithID("confirmation-owner"), session.WithTitle("PROPOSED-ONLY"))
	handle := &confirmationHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, responses: make(chan runtime.InteractionResponse, 4)}
	application := app.New(t.Context(), &confirmationSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	event := &runtime.ToolCallConfirmationEvent{SessionID: sess.ID, RequestID: "confirmation-request-full-ID", ToolCall: tools.ToolCall{ID: "not-executed", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker","task":"STATIC-TASK-PREVIEW"}`}}, ToolDefinition: tools.Tool{Name: subagent.ToolSpawnSubagent}}
	confirmation := dialog.NewToolConfirmationDialog(root.ar, event, root.sessionState)
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	program.Send(dialog.OpenDialogMsg{Model: confirmation})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return s.open && s.active == 0 && strings.Contains(ansi.Strip(s.content), "STATIC-TASK-PREVIEW")
	}, time.Second, time.Millisecond, "proposed preview is static after dialog entrance")
	frame := sidebarProgramSnapshot(t, program)
	require.NotContains(t, ansi.Strip(frame.content), "Unrecognised command")
	require.NotContains(t, ansi.Strip(frame.content), "Awaiting input")
	require.Zero(t, handle.submits.Load(), "preview never executes the proposed tool or admits a turn")
	return program, handle
}

func TestActualProgramToolConfirmationNoEscapeCloseDenyOnce(t *testing.T) {
	for _, gesture := range []string{"default-enter", "N", "escape", "close"} {
		t.Run(gesture, func(t *testing.T) {
			program, handle := startToolConfirmationProgram(t)
			switch gesture {
			case "default-enter":
				program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
			case "N":
				program.Send(tea.KeyPressMsg{Code: 'N', Text: "N"})
			case "escape":
				program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "close":
				frame := sidebarProgramSnapshot(t, program)
				x, y := sidebarProgramPoint(t, frame.content, "✕")
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			select {
			case response := <-handle.responses:
				require.Equal(t, "confirmation-request-full-ID", response.InteractionID)
				require.Equal(t, runtime.InteractionConfirmation, response.Kind)
				require.Equal(t, runtime.ResumeReject(""), response.Resume)
			case <-time.After(time.Second):
				t.Fatal("deny response was not dispatched")
			}
			require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return !s.open && s.active == 0 }, time.Second, time.Millisecond)
			select {
			case extra := <-handle.responses:
				t.Fatalf("duplicate response: %+v", extra)
			default:
			}
		})
	}
}

func TestActualProgramToolReasonCancelReturnsUnansweredThenRejectsOnce(t *testing.T) {
	program, handle := startToolConfirmationProgram(t)
	program.Send(tea.KeyPressMsg{Code: 'R', Text: "R"})
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Why reject")
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return s.open && !s.closing && s.active == 0 && !strings.Contains(ansi.Strip(s.content), "Why reject")
	}, time.Second, time.Millisecond)
	select {
	case response := <-handle.responses:
		t.Fatalf("reason cancel answered parent: %+v", response)
	default:
	}
	program.Send(tea.KeyPressMsg{Code: 'R', Text: "R"})
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Why reject")
	}, time.Second, time.Millisecond)
	program.Send(tea.KeyPressMsg{Code: '1', Text: "1"})
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case response := <-handle.responses:
		require.Equal(t, "confirmation-request-full-ID", response.InteractionID)
		require.Equal(t, runtime.InteractionConfirmation, response.Kind)
		require.Equal(t, runtime.ResumeReject("The arguments provided are incorrect or invalid."), response.Resume, "explicit reason path retains the selected exact reason")
	case <-time.After(time.Second):
		t.Fatal("reason rejection was not dispatched")
	}
	require.Eventually(t, func() bool { s := sidebarProgramSnapshot(t, program); return !s.open && s.active == 0 }, time.Second, time.Millisecond)
	select {
	case response := <-handle.responses:
		t.Fatalf("reason dispatched twice: %+v", response)
	default:
	}
}

func TestToolReasonResultTargetsOnlyCorrelatedConfirmationParent(t *testing.T) {
	root, _ := newTestModel(t)
	root.width, root.height = 120, 40
	matchedEvent := &runtime.ToolCallConfirmationEvent{SessionID: "owner", RequestID: "wanted"}
	otherEvent := &runtime.ToolCallConfirmationEvent{SessionID: "other", RequestID: "other-request"}
	state := &service.SessionState{}
	matched := dialog.NewToolConfirmationDialog(root.ar, matchedEvent, state)
	other := dialog.NewToolConfirmationDialog(root.ar, otherEvent, state)
	root.Update(dialog.OpenDialogMsg{Model: matched})
	root.Update(dialog.OpenDialogMsg{Model: other})
	_, cmd := root.Update(dialog.MultiChoiceResultMsg{DialogID: dialog.ToolRejectionDialogID, Result: dialog.MultiChoiceResult{Value: "specific reason"}, Context: messages.InteractionResponseMsg{SessionID: "owner", Response: runtime.InteractionResponse{InteractionID: "wanted", Kind: runtime.InteractionConfirmation}}})
	found := collectMsgs(cmd)
	closeMsg, ok := firstOfType[dialog.CloseDialogByModelMsg](found)
	require.True(t, ok)
	require.Same(t, matched, closeMsg.Model)
	require.NotSame(t, other, closeMsg.Model)
	response, ok := firstOfType[messages.InteractionResponseMsg](found)
	require.True(t, ok)
	require.Equal(t, "wanted", response.Response.InteractionID)
	require.Equal(t, runtime.ResumeReject("specific reason"), response.Response.Resume)
	root.Update(closeMsg)
	require.Same(t, other, root.dialogMgr.TopDialog(), "a covered confirmation result never closes the unrelated top modal")
}
