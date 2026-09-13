package dialog

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestDialogsProduceCompleteInteractionResponses(t *testing.T) {
	ref := ElicitationRef{SessionID: "session", RequestID: "interaction", ElicitationID: "elicitation"}
	confirmation := NewToolConfirmationDialog(animation.NewRuntime(), &runtime.ToolCallConfirmationEvent{SessionID: "session", RequestID: "interaction"}, &service.SessionState{}).(*toolConfirmationDialog)
	form := NewElicitationDialog("question", nil, nil, ref).(*ElicitationDialog)
	form.responseInput.SetValue("answer")
	elicitation := func(content map[string]any) messages.InteractionResponseMsg {
		return messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{
			InteractionID: "interaction", Kind: runtime.InteractionElicitation, ElicitationID: "elicitation",
			Elicitation: runtime.ElicitationResult{Action: tools.ElicitationActionAccept, Content: content},
		}}
	}
	tests := []struct {
		name string
		cmd  func() tea.Cmd
		want messages.InteractionResponseMsg
	}{
		{name: "confirmation", cmd: func() tea.Cmd { return messageCommand(confirmation.response(runtime.ResumeApprove())) }, want: messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{InteractionID: "interaction", Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeApprove()}}},
		{name: "max", cmd: func() tea.Cmd {
			_, cmd := NewMaxIterationsDialog(10, "session", "interaction").Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			return cmd
		}, want: messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{InteractionID: "interaction", Kind: runtime.InteractionMaxIterations, Resume: runtime.ResumeApprove()}}},
		{name: "form", cmd: func() tea.Cmd { _, cmd := form.submit(); return cmd }, want: elicitation(map[string]any{"response": "answer"})},
		{name: "url", cmd: func() tea.Cmd {
			return NewURLElicitationDialog(t.Context(), "question", "", ref).(*URLElicitationDialog).respond(tools.ElicitationActionAccept)
		}, want: elicitation(nil)},
		{name: "oauth", cmd: func() tea.Cmd {
			_, cmd := NewOAuthAuthorizationDialog("server", ref).(*oauthAuthorizationDialog).respond(tools.ElicitationActionAccept)
			return cmd
		}, want: elicitation(nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got messages.InteractionResponseMsg
			for _, msg := range collectMsgs(test.cmd()) {
				if response, ok := msg.(messages.InteractionResponseMsg); ok {
					got = response
				}
			}
			require.NotEmpty(t, got.SessionID)
			assert.Equal(t, test.want, got)
		})
	}
}

func messageCommand(msg messages.InteractionResponseMsg) tea.Cmd {
	return func() tea.Msg { return msg }
}

// familyActionCell locates painted action text rather than duplicating footer geometry.
func familyActionCell(t *testing.T, d Dialog, label string) (x, y int) {
	t.Helper()
	view := d.View()
	row, col := d.Position()
	lines := strings.Split(ansi.Strip(view), "\n")
	for i, line := range slices.Backward(lines) {
		if before, _, found := strings.Cut(line, label); found {
			return col + ansi.StringWidth(before), row + i
		}
	}
	t.Fatalf("action %q not visible in %q", label, ansi.Strip(view))
	return 0, 0
}

func confirmationFormFamilies(t *testing.T) map[string]func() Dialog {
	t.Helper()
	return map[string]func() Dialog{
		"exit":           NewExitConfirmationDialog,
		"interrupt":      NewInterruptConfirmationDialog,
		"close-root":     func() Dialog { return NewCloseRootWithSubagentsDialog("root") },
		"max-iterations": func() Dialog { return NewMaxIterationsDialog(10, "session", "request") },
		"oauth":          func() Dialog { return NewOAuthAuthorizationDialog("https://example.test/auth", ElicitationRef{}) },
		"url": func() Dialog {
			return NewURLElicitationDialog(t.Context(), "Confirm the action", "https://example.test/auth", ElicitationRef{})
		},
		"tour":        func() Dialog { return NewTourOfferDialog(true) },
		"elicitation": func() Dialog { return NewElicitationDialog("Question", nil, nil, ElicitationRef{}) },
		"mcp-prompt": func() Dialog {
			return NewMCPPromptInputDialog("prompt", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "name"}}})
		},
		"multi-choice": func() Dialog {
			return NewMultiChoiceDialog(MultiChoiceConfig{Title: "Choose", Options: []MultiChoiceOption{{ID: "one", Label: "部署界 e\u0301", Value: "one"}}, AllowCustom: true, AllowSecondary: true})
		},
		"rejection-reason": func() Dialog { return NewToolRejectionReasonDialog("session", "request") },
		"tool-confirmation": func() Dialog {
			return NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
		},
	}
}

func TestConfirmationFormFamiliesWarmThemeAndMouseResources(t *testing.T) { //nolint:paralleltest // In-memory theme globals are restored before parallel tests resume.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	changed := *original
	changed.Colors.BackgroundAlt = "#123456"
	changed.Colors.TextPrimary = "#abcdef"
	for name, factory := range confirmationFormFamilies(t) {
		t.Run(name, func(t *testing.T) {
			styles.ApplyTheme(original)
			d := factory()
			d.SetSize(100, 30)
			warm := d.View()
			assert.Equal(t, warm, d.View(), "warm renders are stable")
			styles.ApplyTheme(&changed)
			fresh := d.View()
			assert.NotEqual(t, warm, fresh, "a warmed dialog must reproject the new palette")
			assert.Equal(t, ansi.Strip(warm), ansi.Strip(fresh), "theme changes preserve cell layout")
			for range 50 {
				_, cmd := d.Update(tea.MouseMotionMsg{X: 0, Y: 0})
				assert.Nil(t, cmd, "idle motion must not schedule work")
			}
			assert.Equal(t, fresh, d.View(), "off-card motion does not mutate content")
			if cleanup, ok := d.(interface{ Cleanup() }); ok {
				cleanup.Cleanup()
			}
		})
	}
}

func TestConfirmationFormFamiliesSmallHeightActionCells(t *testing.T) {
	for name, factory := range confirmationFormFamilies(t) {
		t.Run(name, func(t *testing.T) {
			for _, size := range [][2]int{{100, 30}, {45, 12}, {30, 8}, {20, 6}, {10, 4}} {
				d := factory()
				d.SetSize(size[0], size[1])
				view := d.View()
				row, col := d.Position()
				assert.LessOrEqual(t, lipgloss.Width(view), size[0])
				assert.LessOrEqual(t, lipgloss.Height(view), size[1])
				assert.GreaterOrEqual(t, row, 0)
				assert.GreaterOrEqual(t, col, 0)
				if size[0] >= 20 && size[1] >= 6 {
					actions := d.(interface {
						ActionKeyAt(x, y int, dl DialogLayout) (tea.KeyPressMsg, bool)
					})
					dl := NewDialogLayout(view, row, col)
					count := 0
					for y := row; y < row+lipgloss.Height(view); y++ {
						for x := col; x < col+lipgloss.Width(view); x++ {
							if _, hit := actions.ActionKeyAt(x, y, dl); hit {
								count++
							}
						}
					}
					assert.Positive(t, count, "size %v must retain a clickable action", size)
				}
				if cleanup, ok := d.(interface{ Cleanup() }); ok {
					cleanup.Cleanup()
				}
			}
		})
	}
}

func TestURLBackgroundClickDoesNotOpenBrowser(t *testing.T) {
	d := NewURLElicitationDialog(t.Context(), "Question", "https://example.test/auth", ElicitationRef{})
	d.SetSize(80, 24)
	_ = d.View()
	row, col := d.Position()
	_, cmd := d.Update(tea.MouseClickMsg{X: col, Y: row, Button: tea.MouseLeft})
	assert.Nil(t, cmd, "only the Open action may launch a browser")
}

func TestAuthorizationActionClicksPreserveCorrelation(t *testing.T) {
	ref := ElicitationRef{SessionID: "session", RequestID: "request", ElicitationID: "elicitation"}
	for _, tc := range []struct {
		name, label string
		factory     func() Dialog
		want        tools.ElicitationAction
	}{
		{"oauth-allow", "Authorize", func() Dialog { return NewOAuthAuthorizationDialog("server", ref) }, tools.ElicitationActionAccept},
		{"oauth-deny", "Decline", func() Dialog { return NewOAuthAuthorizationDialog("server", ref) }, tools.ElicitationActionDecline},
		{"url-confirm", "Confirm", func() Dialog { return NewURLElicitationDialog(t.Context(), "Question", "", ref) }, tools.ElicitationActionAccept},
		{"url-decline", "Decline", func() Dialog { return NewURLElicitationDialog(t.Context(), "Question", "", ref) }, tools.ElicitationActionDecline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.factory()
			d.SetSize(45, 12)
			x, y := familyActionCell(t, d, tc.label)
			_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			response := findElicitationResponse(t, cmd)
			assert.Equal(t, ref.SessionID, response.SessionID)
			assert.Equal(t, ref.RequestID, response.Response.InteractionID)
			assert.Equal(t, ref.ElicitationID, response.Response.ElicitationID)
			assert.Equal(t, tc.want, response.Response.Elicitation.Action)
		})
	}
}

func TestMCPPromptFieldsScrollClickAndSubmit(t *testing.T) {
	args := make([]mcptools.PromptArgument, 24)
	for i := range args {
		args[i] = mcptools.PromptArgument{Name: fmt.Sprintf("field-%d", i), Required: true}
	}
	d := NewMCPPromptInputDialog("fields", mcptools.PromptInfo{Description: strings.Repeat("Long description. ", 40), Arguments: args}).(*MCPPromptInputDialog)
	d.SetSize(32, 8)
	_ = d.View()
	for i := range args {
		d.inputs[i].SetValue("value")
		if i > 0 {
			_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		}
		_ = d.View()
	}
	x, y, width, height := d.BodyScrollBounds()
	line := d.fieldStarts[len(args)-1] + d.fieldHeights[len(args)-1] - 1
	require.Positive(t, height)
	require.Greater(t, width, 2)
	require.GreaterOrEqual(t, line, d.BodyScrollOffset())
	require.Less(t, line, d.BodyScrollOffset()+height, "last input remains keyboard reachable")
	_, _ = d.Update(tea.MouseClickMsg{X: x, Y: y + line - d.BodyScrollOffset(), Button: tea.MouseLeft})
	assert.Equal(t, len(args)-1, d.currentInput)
	_, _ = d.Update(tea.KeyPressMsg{Code: '界', Text: "界"})
	assert.Contains(t, d.inputs[len(args)-1].Value(), "界")
	buttonX, buttonY := familyActionCell(t, d, "Execute")
	_, cmd := d.Update(tea.MouseClickMsg{X: buttonX, Y: buttonY, Button: tea.MouseLeft})
	response, ok := findMsg[messages.MCPPromptMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Len(t, response.Arguments, len(args), "all prompt parameters survive scrolling")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd, "execute responds once")
}

func TestMultiChoiceAllOptionsAndCustomRemainReachable(t *testing.T) {
	options := make([]MultiChoiceOption, 32)
	for i := range options {
		options[i] = MultiChoiceOption{ID: strconv.Itoa(i), Label: fmt.Sprintf("部署 %d e\u0301", i), Value: strconv.Itoa(i)}
	}
	d := NewMultiChoiceDialog(MultiChoiceConfig{Title: "Choose", Options: options, AllowCustom: true}).(*multiChoiceDialog)
	d.SetSize(30, 8)
	_ = d.View()
	for range len(options) + 1 {
		_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		_ = d.View()
	}
	require.Equal(t, selectionCustom, d.selected)
	x, y, _, height := d.BodyScrollBounds()
	custom := d.clickables[len(d.clickables)-1]
	require.Positive(t, height)
	require.GreaterOrEqual(t, custom.startRow, d.BodyScrollOffset())
	require.Less(t, custom.startRow, d.BodyScrollOffset()+height)
	_, _ = d.Update(tea.MouseClickMsg{X: x + 5, Y: y + custom.startRow - d.BodyScrollOffset(), Button: tea.MouseLeft})
	assert.Equal(t, selectionCustom, d.selected, "clicking the custom editor focuses rather than deselects it")
	_, _ = d.Update(tea.PasteMsg{Content: "custom answer"})
	buttonX, buttonY := familyActionCell(t, d, "Continue")
	_, cmd := d.Update(tea.MouseClickMsg{X: buttonX, Y: buttonY, Button: tea.MouseLeft})
	response, ok := findMsg[MultiChoiceResultMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, "custom answer", response.Result.Value)
	assert.True(t, response.Result.IsCustom)
}

func TestElicitationFreeFormTypingRevealsInputAndKeepsCursorKeys(t *testing.T) {
	d := NewElicitationDialog(strings.Repeat("Question text. ", 100), nil, nil, ElicitationRef{}).(*ElicitationDialog)
	d.SetSize(30, 8)
	_ = d.View()
	_, _ = d.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	_ = d.View()
	assert.Positive(t, d.BodyScrollOffset(), "typing reveals the free-form response")
	x, y, _, height := d.BodyScrollBounds()
	last := len(d.layout().bodyLines) - 1
	require.Less(t, last, d.BodyScrollOffset()+height)
	_, _ = d.Update(tea.MouseClickMsg{X: x, Y: y + last - d.BodyScrollOffset(), Button: tea.MouseLeft})
	_, _ = d.Update(tea.KeyPressMsg{Code: '界', Text: "界"})
	assert.Equal(t, "界a", d.responseInput.Value(), "text clicks place the cursor in terminal cells")
	before := d.BodyScrollOffset()
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Equal(t, before, d.BodyScrollOffset(), "Home belongs to the text editor, not body scrolling")
}

func TestConfirmationProseWrapsAtBodyWidthOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func() Dialog
		prose   string
	}{
		{"close-root", func() Dialog { return NewCloseRootWithSubagentsDialog("root") }, "This session has running subagents. Closing it will interrupt their current work and close their tabs. Continue?"},
		{"exit", NewExitConfirmationDialog, "Do you want to exit?"},
		{"interrupt", NewInterruptConfirmationDialog, "Stop the current response?"},
		{"max", func() Dialog { return NewMaxIterationsDialog(10, "session", "request") }, "The agent may be stuck in a loop. This can happen with smaller or less capable models."},
		{"oauth", func() Dialog { return NewOAuthAuthorizationDialog("server", ElicitationRef{}) }, "This server requires OAuth authentication to access its tools. Your browser will open automatically to complete the authorization process."},
		{"url", func() Dialog {
			return NewURLElicitationDialog(t.Context(), "Please complete authorization before continuing with the requested action.", "", ElicitationRef{})
		}, "Please complete authorization before continuing with the requested action."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, width := range []int{45, 80, 120} {
				d := tc.factory()
				d.SetSize(width, 100)
				view := d.View()
				row, col := d.Position()
				body := d.(interface{ BodyScrollBounds() (int, int, int, int) })
				x, y, bodyWidth, height := body.BodyScrollBounds()
				lines := strings.Split(ansi.Strip(view), "\n")
				var text []string
				for _, line := range lines[y-row : y-row+height] {
					text = append(text, ansi.Cut(line, x-col, x-col+max(1, bodyWidth-2)))
				}
				assert.Contains(t, strings.Join(strings.Fields(strings.Join(text, " ")), " "), tc.prose,
					"width %d: reserving scrollbar cells must not split already wrapped words", width)
			}
		})
	}
}

func TestFormViewsPreservePreparedInteractionState(t *testing.T) {
	type formCase struct {
		name  string
		model Dialog
		state func() any
	}
	for _, tc := range []formCase{
		func() formCase {
			d := NewElicitationDialog("Question", nil, nil, ElicitationRef{}).(*ElicitationDialog)
			return formCase{"elicitation", d, func() any {
				return []any{d.reanchorFocus, d.scrollableRow, d.scrollableCol, d.responseInput.Position(), d.responseInput.Focused(), len(d.scrollviews), d.BodyScrollOffset(), d.bodyScroll.VisualGeneration()}
			}}
		}(),
		func() formCase {
			d := NewMCPPromptInputDialog("prompt", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "name"}}}).(*MCPPromptInputDialog)
			return formCase{"mcp", d, func() any {
				return []any{d.reanchorFocus, d.bodyRow, d.bodyCol, d.currentInput, d.inputs[0].Position(), d.inputs[0].Focused(), len(d.scrollviews), d.BodyScrollOffset(), d.bodyScroll.VisualGeneration()}
			}}
		}(),
		func() formCase {
			d := NewMultiChoiceDialog(MultiChoiceConfig{Title: "Choose", AllowCustom: true}).(*multiChoiceDialog)
			return formCase{"multi-choice", d, func() any {
				return []any{d.reanchorFocus, d.contentAbsRow, d.contentAbsCol, d.selected, d.tabOverride, d.customInput.Position(), d.customInput.Focused(), len(d.scrollviews), d.BodyScrollOffset(), d.bodyScroll.VisualGeneration()}
			}}
		}(),
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.model.SetSize(40, 12)
			_, _ = tc.model.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
			before := tc.state()
			view := tc.model.View()
			for range 5 {
				assert.Equal(t, view, tc.model.View())
				_, _ = tc.model.Position()
			}
			assert.Equal(t, before, tc.state(), "rendering must not change prepared focus, geometry, scroll generation, or registrations")
		})
	}
}
