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
	"github.com/docker/docker-agent/pkg/tui/widgets/textinput"
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
		{"close-root", func() Dialog { return NewCloseRootWithSubagentsDialog("root") }, "Close this tab? Subagents keep running and their open tabs stay open."},
		{"exit", NewExitConfirmationDialog, "Do you want to exit?"},
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

func TestMCPExecuteAvailabilityMatchesRequiredFields(t *testing.T) {
	d := NewMCPPromptInputDialog("required", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "name", Required: true}}}).(*MCPPromptInputDialog)
	d.SetSize(70, 20)
	x, y := familyActionCell(t, d, "Execute")
	_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd, "required empty input disables Execute")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd, "keyboard obeys the same required-input guard")
	_, _ = d.Update(tea.PasteMsg{Content: "value"})
	x, y = familyActionCell(t, d, "Execute")
	_, cmd = d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	response, ok := findMsg[messages.MCPPromptMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, "value", response.Arguments["name"])
}

func TestMultiChoiceDisabledPrimaryAndAbsentSecondaryFocus(t *testing.T) {
	d := NewMultiChoiceDialog(MultiChoiceConfig{Title: "Choose", Options: []MultiChoiceOption{{ID: "one", Label: "One", Value: "value"}}}).(*multiChoiceDialog)
	d.SetSize(70, 20)
	x, y := familyActionCell(t, d, "Continue")
	_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd, "Continue cannot submit without a selection")
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.False(t, d.isSecondaryDefault(), "Tab cannot focus an absent secondary action")
	assert.NotContains(t, ansi.Strip(d.View()), "Skip")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	result, ok := findMsg[MultiChoiceResultMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, "one", result.Result.OptionID)
	assert.Equal(t, "value", result.Result.Value)
}

func TestElicitationSubmitRemainsAvailableForValidation(t *testing.T) {
	d := NewElicitationDialog("Question", map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []any{"name"}}, nil, ElicitationRef{}).(*ElicitationDialog)
	d.SetSize(70, 20)
	x, y := familyActionCell(t, d, "Submit")
	_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, cmd)
	assert.NotEmpty(t, d.fieldErrors, "Submit performs useful validation even when the form is incomplete")
	assert.False(t, d.responseSent, "validation must not answer the runtime waiter")
}

func TestOwnedFamiliesTitleBodyGapDoesNotEnterScrollableContent(t *testing.T) {
	for name, factory := range confirmationFormFamilies(t) {
		t.Run(name, func(t *testing.T) {
			d := factory()
			d.SetSize(100, 40)
			view := d.View()
			row, col := d.Position()
			body := d.(interface {
				BodyScrollBounds() (int, int, int, int)
				BodyScrollOffset() int
			})
			x, y, width, height := body.BodyScrollBounds()
			require.Positive(t, height)
			require.Positive(t, y-row)
			lines := strings.Split(ansi.Strip(view), "\n")
			gap := strings.TrimSpace(ansi.Cut(lines[y-row-1], x-col, x-col+max(1, width-2)))
			assert.Empty(t, gap, "shared title gap is outside the body's first row")
			assert.Zero(t, body.BodyScrollOffset())
			if cleanup, ok := d.(interface{ Cleanup() }); ok {
				cleanup.Cleanup()
			}
		})
	}
}

func TestFormActionBoundariesDoNotStealInputKeys(t *testing.T) {
	mcp := NewMCPPromptInputDialog("fields", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "first"}, {Name: "second"}}}).(*MCPPromptInputDialog)
	mcp.SetSize(70, 20)
	_, _ = mcp.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	_, _ = mcp.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	_, _ = mcp.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	assert.Equal(t, "ba", mcp.inputs[0].Value())
	_, _ = mcp.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, 1, mcp.currentInput)
	assert.False(t, mcp.ActionsFocused())
	_, _ = mcp.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.True(t, mcp.ActionsFocused())
	_, _ = mcp.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	assert.Empty(t, mcp.inputs[1].Value(), "action focus does not type into blurred fields")
	_, _ = mcp.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.False(t, mcp.ActionsFocused())
	assert.Equal(t, 1, mcp.currentInput)
	assert.True(t, mcp.inputs[1].Focused())

	form := NewElicitationDialog("Question", nil, nil, ElicitationRef{SessionID: "session", RequestID: "request"}).(*ElicitationDialog)
	form.SetSize(70, 20)
	_, _ = form.Update(tea.PasteMsg{Content: "answer"})
	_, _ = form.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.True(t, form.ActionsFocused())
	_, _ = form.Update(tea.PasteMsg{Content: "ignored"})
	assert.Equal(t, "answer", form.responseInput.Value())
	_, cmd := form.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	response := findElicitationResponse(t, cmd)
	assert.Equal(t, "request", response.Response.InteractionID)
	assert.Equal(t, map[string]any{"response": "answer"}, response.Response.Elicitation.Content)
}

func TestDecisionActionFocusRoutesOriginalKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func() Dialog
		want    any
	}{
		{"exit", NewExitConfirmationDialog, ExitConfirmedMsg{}},
		{"close-root", func() Dialog { return NewCloseRootWithSubagentsDialog("root") }, CloseRootWithSubagentsConfirmedMsg{SessionID: "root"}},
		{"tour", func() Dialog { return NewTourOfferDialog(false) }, TourOfferResultMsg{Choice: TourOfferAccepted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.factory()
			d.SetSize(80, 24)
			_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			assert.Contains(t, collectMsgs(cmd), tc.want)
		})
	}
	ref := ElicitationRef{SessionID: "session", RequestID: "request", ElicitationID: "elicitation"}
	for _, factory := range []func() Dialog{
		func() Dialog { return NewOAuthAuthorizationDialog("server", ref) },
		func() Dialog { return NewURLElicitationDialog(t.Context(), "Question", "", ref) },
	} {
		d := factory()
		d.SetSize(80, 24)
		_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		response := findElicitationResponse(t, cmd)
		assert.Equal(t, "session", response.SessionID)
		assert.Equal(t, "request", response.Response.InteractionID)
		assert.Equal(t, tools.ElicitationActionAccept, response.Response.Elicitation.Action)
	}
}

func TestRuntimeDecisionEscapeAndCloseRejectExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func() Dialog
		kind    runtime.InteractionKind
	}{
		{"max", func() Dialog { return NewMaxIterationsDialog(10, "session", "request") }, runtime.InteractionMaxIterations},
		{"tool", func() Dialog {
			return NewToolConfirmationDialog(animation.NewRuntime(), &runtime.ToolCallConfirmationEvent{SessionID: "session", RequestID: "request"}, &service.SessionState{})
		}, runtime.InteractionConfirmation},
	} {
		for _, path := range []string{"escape", "close"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				d := tc.factory()
				d.SetSize(80, 24)
				outside := d.(interface{ OutsideClickDismissCmd() tea.Cmd })
				assert.Nil(t, outside.OutsideClickDismissCmd(), "outside clicks cannot answer a runtime decision")
				var cmd tea.Cmd
				if path == "escape" {
					_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				} else {
					view := d.View()
					row, col := d.Position()
					dl := NewDialogLayout(view, row, col)
					chrome := d.(interface {
						CloseButtonHit(msg tea.MouseClickMsg, dl DialogLayout) bool
					})
					found := false
					for y := row; y < row+lipgloss.Height(view) && !found; y++ {
						for x := col; x < col+lipgloss.Width(view); x++ {
							click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
							if chrome.CloseButtonHit(click, dl) {
								_, cmd = d.Update(click)
								found = true
								break
							}
						}
					}
					require.True(t, found)
				}
				response, ok := findMsg[messages.InteractionResponseMsg](collectMsgs(cmd))
				require.True(t, ok)
				assert.Equal(t, "session", response.SessionID)
				assert.Equal(t, "request", response.Response.InteractionID)
				assert.Equal(t, tc.kind, response.Response.Kind)
				assert.Equal(t, runtime.ResumeReject(""), response.Response.Resume)
				_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				assert.Nil(t, cmd)
			})
		}
	}
}

func TestConfirmationSelectionEnterHintTracksArrowAndResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func() Dialog
		yes     any
	}{
		{"exit", NewExitConfirmationDialog, ExitConfirmedMsg{}},
		{"close-root", func() Dialog { return NewCloseRootWithSubagentsDialog("root") }, CloseRootWithSubagentsConfirmedMsg{SessionID: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.factory()
			d.SetSize(100, 30)
			initial := ansi.Strip(d.View())
			assert.Equal(t, 1, strings.Count(initial, "↵"))
			assert.Contains(t, initial, "No ↵")
			assert.NotContains(t, initial, "No ↵ n")
			_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
			after := ansi.Strip(d.View())
			assert.Equal(t, 1, strings.Count(after, "↵"))
			assert.NotContains(t, after, "No ↵")
			assert.NotEqual(t, initial, after)
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			assert.Contains(t, collectMsgs(cmd), tc.yes)
		})
	}
}

func TestFormDefaultEnterMarkerMovesOnlyWithActionFocus(t *testing.T) {
	d := NewMultiChoiceDialog(MultiChoiceConfig{Title: "Choose", Options: []MultiChoiceOption{{ID: "one", Label: "One", Value: "value"}}, AllowSecondary: true}).(*multiChoiceDialog)
	d.SetSize(100, 30)
	_, _ = d.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	initial := ansi.Strip(d.View())
	assert.Equal(t, 1, strings.Count(initial, "↵"))
	assert.NotContains(t, initial, "Skip ↵")
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	focused := ansi.Strip(d.View())
	assert.Equal(t, 1, strings.Count(focused, "↵"))
	assert.Contains(t, focused, "Skip")
	assert.NotEqual(t, initial, focused)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	result, ok := findMsg[MultiChoiceResultMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.True(t, result.Result.IsSkipped)
}

func TestMCPPromptSharedFooterGapDoesNotDoubleFieldSpacing(t *testing.T) {
	d := NewMCPPromptInputDialog("fields", mcptools.PromptInfo{Arguments: []mcptools.PromptArgument{{Name: "first"}, {Name: "second"}}}).(*MCPPromptInputDialog)
	d.SetSize(80, 24)
	width, _ := d.mcpPromptDialogDimensions()
	body, starts, heights := d.buildBody(d.BodyContentWidth(width))
	lines := strings.Split(ansi.Strip(body), "\n")
	require.Equal(t, []int{0, 3}, starts)
	require.Equal(t, []int{2, 2}, heights)
	require.Len(t, lines, 5, "inter-field gap remains; final field does not add a footer spacer")
	require.Empty(t, strings.TrimSpace(lines[2]))
	require.Equal(t, 1, d.bodyFooterGap)
	require.Equal(t, 5, d.bodyHeight)
}

func TestFormInitialFocusCursorVisibleBeforeRenderAndAfterResize(t *testing.T) {
	for _, family := range []string{"mcp", "elicitation-free", "elicitation-schema"} {
		for _, initial := range [][2]int{{100, 30}, {45, 12}, {20, 6}} {
			t.Run(fmt.Sprintf("%s/%dx%d", family, initial[0], initial[1]), func(t *testing.T) {
				var d Dialog
				switch family {
				case "mcp":
					d = NewMCPPromptInputDialog("prompt", mcptools.PromptInfo{Description: strings.Repeat("Description. ", 16), Arguments: []mcptools.PromptArgument{{Name: "name"}}})
				case "elicitation-free":
					d = NewElicitationDialog("Question", nil, nil, ElicitationRef{})
				default:
					d = NewElicitationDialog("Question", map[string]any{"type": "string", "title": "Name"}, nil, ElicitationRef{})
				}
				var b *BaseDialog
				var input *textinput.Model
				var focusLine func() int
				switch model := d.(type) {
				case *MCPPromptInputDialog:
					b, input = &model.BaseDialog, &model.inputs[0]
					focusLine = func() int { return model.fieldStarts[0] + model.fieldHeights[0] - 1 }
				case *ElicitationDialog:
					b = &model.BaseDialog
					if model.hasFreeFormInput() {
						input = &model.responseInput
						focusLine = func() int { return len(model.layout().bodyLines) - 1 }
					} else {
						input = &model.inputs[0]
						focusLine = func() int {
							start, _ := model.focusRange()
							return start
						}
					}
				}
				input.SetValue("ZX")
				input.SetCursor(0)
				for _, size := range [][2]int{initial, {20, 6}, {100, 30}, {45, 12}} {
					d.SetSize(size[0], size[1])
					x, y, _, height := b.BodyScrollBounds()
					line := focusLine()
					offset := b.BodyScrollOffset()
					require.GreaterOrEqual(t, line, offset, "focused row is prepared before View")
					require.Less(t, line, offset+height)
					view := d.View()
					row, col := d.Position()
					inputY := y + line - offset
					lines := strings.Split(view, "\n")
					require.Contains(t, ansi.Strip(lines[inputY-row]), "ZX")
					require.Regexp(t, `\x1b\[[0-9;]*\b7(?:;[0-9;]*)?mZ`, lines[inputY-row], "actual focused cursor remains visible, not just its row")
					require.Equal(t, offset, b.BodyScrollOffset(), "View never reanchors focus")
					require.LessOrEqual(t, col+lipgloss.Width(view), size[0])
					require.LessOrEqual(t, row+lipgloss.Height(view), size[1])
					// Returned cursor blink commands are deliberately not executed.
					_, _ = d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x + lipgloss.Width(input.Prompt) + 1, Y: inputY})
					require.Equal(t, 1, input.Position(), "painted input row maps to the actual cursor cell")
					input.SetCursor(0)
				}
			})
		}
	}
}

func TestFormFocusPreparationDoesNotOverrideManualScrollAtSameSize(t *testing.T) {
	forms := []Dialog{
		NewMCPPromptInputDialog("prompt", mcptools.PromptInfo{Description: strings.Repeat("Description. ", 30), Arguments: []mcptools.PromptArgument{{Name: "name"}}}),
		NewElicitationDialog(strings.Repeat("Question. ", 30), nil, nil, ElicitationRef{}),
	}
	for _, d := range forms {
		d.SetSize(45, 12)
		var b *BaseDialog
		switch model := d.(type) {
		case *MCPPromptInputDialog:
			b = &model.BaseDialog
		case *ElicitationDialog:
			b = &model.BaseDialog
		}
		require.Positive(t, b.BodyScrollOffset(), "initial preparation reveals focus below long prose")
		b.bodyScroll.ScrollToTop()
		d.SetSize(45, 12)
		d.View()
		require.Zero(t, b.BodyScrollOffset(), "same-size preparation and paint preserve deliberate manual scrolling")
	}
}
