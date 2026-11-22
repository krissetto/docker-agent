package dialog

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestExitDialogPills(t *testing.T) {
	t.Parallel()
	d := NewExitConfirmationDialog()
	d.SetSize(80, 25)
	assert.Contains(t, ansi.Strip(d.View()), "No ↵")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Nil(t, cmd)
	assert.Contains(t, ansi.Strip(d.View()), "Yes ↵")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Nil(t, cmd)
	assert.Contains(t, ansi.Strip(d.View()), "No ↵")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, []tea.Msg{CloseDialogMsg{}}, collectMsgs(cmd))
}

func TestDialogShellCancelsWithoutActivatingTheBody(t *testing.T) {
	t.Parallel()
	for _, closeButton := range []bool{false, true} {
		for _, family := range []string{"exit", "elicitation", "url", "oauth", "tool", "max iterations"} {
			name := family + "/outside"
			if closeButton {
				name = family + "/close"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				mgr := New().(*manager)
				mgr.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
				var model Dialog
				var event tea.Msg
				responder := &recordingElicitationResponder{}
				const id = "request-1"
				switch family {
				case "exit":
					model = NewExitConfirmationDialog()
				case "elicitation":
					model = NewElicitationDialog("Choose a value", nil, nil, id)
					event = &runtime.ElicitationRequestEvent{ElicitationID: id}
				case "url":
					model = NewURLElicitationDialog(t.Context(), "Choose a value", "https://example.invalid", id)
					event = &runtime.ElicitationRequestEvent{ElicitationID: id}
				case "oauth":
					model = NewOAuthAuthorizationDialog(t.Context(), "https://example.invalid", responder, id)
					event = &runtime.ElicitationRequestEvent{ElicitationID: id}
				case "tool":
					model = NewToolConfirmationDialog(animation.NewRuntime(), &runtime.ToolCallConfirmationEvent{
						ToolCall:       tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "read_file", Arguments: `{}`}},
						ToolDefinition: tools.Tool{Name: "read_file"},
					}, &service.EmbeddedSessionState{})
				case "max iterations":
					model = NewMaxIterationsDialog(5)
				}
				mgr.Update(OpenDialogMsg{Model: NewHelpDialog(nil)})
				mgr.Update(OpenDialogMsg{Model: model, OriginatingEvent: event})
				mgr.stack[1].offsetX, mgr.stack[1].offsetY = 2, 1
				layer := mgr.GetLayers()[1]
				lines := strings.Split(ansi.Strip(layer.GetContent()), "\n")
				assert.Contains(t, lines[1], closeGlyph)
				assert.Contains(t, lines[3], "────")
				click := tea.MouseClickMsg{Button: tea.MouseLeft}
				if closeButton {
					click.X = layer.GetX() + lipgloss.Width(lines[1]) - 3
					click.Y = layer.GetY() + 1
				}
				_, cmd := mgr.Update(click)
				if !closeButton && (family == "tool" || family == "max iterations") {
					require.Nil(t, cmd, "mandatory prompts ignore outside clicks")
					require.Same(t, model, mgr.TopDialog())
					return
				}
				require.NotNil(t, cmd)
				_, duplicate := mgr.Update(click)
				require.Nil(t, duplicate)
				msgs := collectMsgs(cmd)
				for _, msg := range msgs {
					mgr.Update(msg)
				}
				require.IsType(t, &helpDialog{}, mgr.TopDialog())
				switch family {
				case "exit":
					assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, msgs)
				case "elicitation", "url":
					response, ok := firstMsgOfType[messages.ElicitationResponseMsg](msgs)
					require.True(t, ok)
					assert.Equal(t, id, response.ElicitationID)
					assert.Equal(t, tools.ElicitationActionCancel, response.Action)
				case "oauth":
					assert.Equal(t, 1, responder.calls)
					assert.Equal(t, id, responder.id)
					assert.Equal(t, tools.ElicitationActionDecline, responder.action)
				case "tool", "max iterations":
					resume, ok := firstMsgOfType[RuntimeResumeMsg](msgs)
					require.True(t, ok)
					assert.Equal(t, runtime.ResumeReject(""), resume.Request)
				}
			})
		}
	}
}

func TestDialogChromeFrames(t *testing.T) {
	t.Parallel()
	clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
	mgr := NewWithAnimation(clock).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	mgr.Update(OpenDialogMsg{Model: NewExitConfirmationDialog()})
	require.Equal(t, 1, lipgloss.Height(mgr.GetLayers()[0].GetContent()))
	finishDialogAnimation(t, mgr, clock)
	frame := mgr.GetLayers()[0].GetContent()
	require.Greater(t, lipgloss.Height(frame), 1)
	require.Contains(t, frame, closeGlyph)
	mgr.Update(CloseDialogMsg{})
	require.False(t, mgr.Open(), "visual close does not delay logical dismissal")
	require.Equal(t, frame, mgr.GetLayers()[0].GetContent())
	finishDialogAnimation(t, mgr, clock)
	require.Empty(t, mgr.GetLayers())
}

func TestDialogChromeRendering(t *testing.T) {
	t.Parallel()
	mgr := New().(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, d := range []Dialog{
		NewExitConfirmationDialog(),
		NewElicitationDialog("Enter a value", nil, nil),
		NewURLElicitationDialog(t.Context(), "Please visit the URL", "https://example.invalid"),
		NewSettingsDialog(messages.Preferences{}, true),
	} {
		mgr.Update(OpenDialogMsg{Model: d})
		view := ansi.Strip(mgr.View())
		t.Logf("\n%s", view)
		lower := strings.ToLower(view)
		assert.NotContains(t, lower, "esc cancel")
		assert.NotContains(t, lower, "pgup/pgdn scroll")
		assert.Contains(t, view, closeGlyph)
		mgr.Update(CloseDialogMsg{})
	}
}

func TestElicitationCancelPill(t *testing.T) {
	t.Parallel()
	for _, useMouse := range []bool{false, true} {
		d := NewElicitationDialog("Enter a value", nil, nil, "request-1")
		d.SetSize(100, 40)
		var cmd tea.Cmd
		if useMouse {
			row, col := d.Position()
			for i, line := range strings.Split(ansi.Strip(d.View()), "\n") {
				if at := strings.Index(line, "Cancel"); at >= 0 {
					_, cmd = d.Update(tea.MouseClickMsg{X: col + lipgloss.Width(line[:at]), Y: row + i, Button: tea.MouseLeft})
					break
				}
			}
		} else {
			d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			require.Contains(t, ansi.Strip(d.View()), "Cancel ↵")
			_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		}
		response, ok := firstMsgOfType[messages.ElicitationResponseMsg](collectMsgs(cmd))
		require.True(t, ok)
		assert.Equal(t, "request-1", response.ElicitationID)
		assert.Equal(t, tools.ElicitationActionCancel, response.Action)
	}
}
