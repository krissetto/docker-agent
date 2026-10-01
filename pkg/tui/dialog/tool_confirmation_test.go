package dialog

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newConfirmationEvent builds a tool-call confirmation event carrying the
// supplied metadata for use in the dialog tests.
func newConfirmationEvent(metadata map[string]string) *runtime.ToolCallConfirmationEvent {
	return &runtime.ToolCallConfirmationEvent{
		Type:           "tool_call_confirmation",
		ToolCall:       tools.ToolCall{ID: "x", Function: tools.FunctionCall{Name: "shell", Arguments: "{}"}},
		ToolDefinition: tools.Tool{Name: "shell"},
		Metadata:       metadata,
	}
}

func realShellLSConfirmationEvent() *runtime.ToolCallConfirmationEvent {
	return &runtime.ToolCallConfirmationEvent{
		Type: "tool_call_confirmation",
		ToolCall: tools.ToolCall{
			ID: "call_ls",
			Function: tools.FunctionCall{
				Name:      "shell",
				Arguments: `{"cmd":"ls"}`,
			},
		},
		ToolDefinition: tools.Tool{Name: "shell"},
	}
}

func TestToolConfirmationDialog_CompactRealShellLSGeometry(t *testing.T) {
	const width, height = 165, 47
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), realShellLSConfirmationEvent(), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: width, Height: height})

	view := dialog.View()
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "Proposed: shell")
	assert.Contains(t, plain, "cmd:")
	assert.Contains(t, plain, "ls")
	assert.Less(t, lipgloss.Height(view), height,
		"a short proposed call stays content-sized with its inputs and choices")
	row, col := dialog.Position()
	assert.Equal(t, (height-lipgloss.Height(view))/2, row)
	assert.Equal(t, (width-lipgloss.Width(view))/2, col)
}

func TestToolConfirmationDialog_RendersMetadata(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{"danger": "high", "reason": "policy-x"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Metadata")
	assert.Contains(t, view, "danger: high")
	assert.Contains(t, view, "reason: policy-x")
}

func TestToolConfirmationDialog_NoMetadataSection_WhenEmpty(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.NotContains(t, view, "Metadata")
}

func TestToolConfirmationDialog_MetadataKeysSorted(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{"zebra": "1", "apple": "2", "mango": "3"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	apple := strings.Index(view, "apple:")
	mango := strings.Index(view, "mango:")
	zebra := strings.Index(view, "zebra:")
	require.NotEqual(t, -1, apple)
	require.NotEqual(t, -1, mango)
	require.NotEqual(t, -1, zebra)
	assert.Less(t, apple, mango, "keys must render in sorted order")
	assert.Less(t, mango, zebra, "keys must render in sorted order")
}

// TestToolConfirmationDialog_RendersSafetyWarning pins the destructive-
// command UX: when the confirmation event carries `blast_radius`
// assessment metadata, the dialog composes a readable
// warning block instead of rendering raw key/value pairs. The
// convention keys (blast_radius, category, reason) are suppressed
// from the plain Metadata section.
func TestToolConfirmationDialog_RendersSafetyWarning(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{
			"blast_radius": "high",
			"category":     "fs-delete",
			"reason":       "Command matches destructive operation: rm -rf <path>",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Destructive command", "warning block must name what it's warning about")
	assert.Contains(t, view, "high blast radius", "warning block must name the severity in prose")
	assert.Contains(t, view, "Command matches destructive operation: rm -rf <path>",
		"the matched-pattern reason must surface as supporting context")
	assert.NotContains(t, view, "blast_radius:",
		"raw blast_radius key must not appear; the warning block replaces it")
	assert.NotContains(t, view, "category: fs-delete",
		"raw category key must not appear when blast_radius is in play")
	assert.NotContains(t, view, "Metadata",
		"the plain Metadata section must not render when only convention keys are present")
}

// The runtime attaches safety_label to every confirmation for API
// consumers; the dialog must not leak it as a raw metadata row.
func TestToolConfirmationDialog_SafetyLabelNeverRendersRaw(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{
			"safety_label": "destructive",
			"blast_radius": "high",
			"reason":       "Command matches destructive operation: rm -rf <path>",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Destructive command")
	assert.NotContains(t, view, "safety_label:",
		"programmatic classifier state must not render as a raw pair")
}

// An unrecognised command must not be presented as destructive: crying
// wolf on every unlisted command would erode trust in real warnings.
func TestToolConfirmationDialog_UnknownRadiusIsNotDestructive(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{
			"safety_label": "unknown",
			"blast_radius": "unknown",
			"reason":       "Shell command is not positively recognised by the safety classifier.",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Unrecognised command")
	assert.NotContains(t, view, "Destructive command")
}

// TestToolConfirmationDialog_RendersSafetyWarningPlusExtraMetadata
// covers the case where a permission_request hook contributes its own
// metadata alongside an assessment. The warning block uses
// the assessment convention keys, and the extra keys still render
// as plain pairs in the Metadata section.
func TestToolConfirmationDialog_RendersSafetyWarningPlusExtraMetadata(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{
			"blast_radius": "medium",
			"reason":       "rm without recursion flag",
			"team_policy":  "review-required",
			"ticket":       "SEC-1234",
		}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "medium blast radius", "warning block consumes blast_radius")
	assert.Contains(t, view, "rm without recursion flag", "warning block consumes reason")
	assert.Contains(t, view, "team_policy: review-required",
		"non-convention keys still render as plain pairs")
	assert.Contains(t, view, "ticket: SEC-1234")
}

// TestToolConfirmationDialog_ReasonOutsideSafetyVerdictRendersPlain
// pins the orthogonality: the `reason` key is generic and can be
// used by permission_request hooks for unrelated purposes. When
// blast_radius is NOT present, reason renders as a plain pair so
// existing permission_request consumers aren't affected.
func TestToolConfirmationDialog_ReasonOutsideSafetyVerdictRendersPlain(t *testing.T) {
	t.Parallel()

	dialog := NewToolConfirmationDialog(animation.NewRuntime(),
		newConfirmationEvent(map[string]string{"reason": "policy-x"}),
		&service.SessionState{},
	)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "Metadata")
	assert.Contains(t, view, "reason: policy-x",
		"without blast_radius, reason is just a regular metadata key")
	assert.NotContains(t, view, "Destructive command",
		"no warning block without a blast_radius classification")
}

// The dialog must work with an embedder-provided session state, not just the
// full application's *service.SessionState; the "all tools" decision flips
// the embedder's session-wide approval.
func TestToolConfirmationDialog_EmbeddedSessionState(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{}
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	view := ansi.Strip(dialog.View())
	assert.Contains(t, view, "shell")
	assert.Contains(t, view, "Do you want to allow this tool call?")

	require.False(t, state.YoloMode())
	_, cmd := dialog.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.NotNil(t, cmd)
	assert.True(t, state.YoloMode(), "approving all tools must flip the embedder's session-wide approval")
}

// locateInView finds needle in the rendered dialog and returns the
// absolute screen row and column (terminal cells) of its first cell.
// The prefix before needle is measured in cells, not bytes, because
// the dialog border rune is multi-byte and mouse coordinates are cells.
func locateInView(d *toolConfirmationDialog, needle string) (y, x int, ok bool) {
	dialogRow, dialogCol := d.Position()
	for i, line := range strings.Split(ansi.Strip(d.View()), "\n") {
		if before, _, found := strings.Cut(line, needle); found {
			return dialogRow + i, dialogCol + ansi.StringWidth(before), true
		}
	}
	return 0, 0, false
}

// clickCell probes each hitbox with a fresh decision lifecycle.
func clickCell(d *toolConfirmationDialog, y, x int) tea.Cmd {
	probe := NewToolConfirmationDialog(animation.NewRuntime(), d.msg, d.sessionState).(*toolConfirmationDialog)
	probe.SetSize(d.Width(), d.Height())
	probe.actionsFocused, probe.focusedAction = d.actionsFocused, d.focusedAction
	probe.prepareLayout()
	probe.bodyScroll.SetScrollOffset(d.BodyScrollOffset())
	_, cmd := probe.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

// Clicking the leading action key must fire at every dialog width,
// including widths where the options block wraps onto several rows.
// RenderHelpKeys bakes lipgloss centering spaces into each line;
// hit-testing must factor them out or 'Y' clicks dispatch on a padding
// space and silently no-op whenever the centering padding is odd.
func TestToolConfirmationDialog_ClickOnYFiresAtEveryWidth(t *testing.T) {
	t.Parallel()

	for width := 60; width <= 130; width++ {
		dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{})
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: width, Height: 30})

		d, ok := dialog.(*toolConfirmationDialog)
		require.True(t, ok)

		y, x, found := locateInView(d, "Yes, once")
		require.Truef(t, found, "width %d: 'Yes, once' must be visible on some options row", width)

		resume, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(clickCell(d, y, x)))
		require.Truef(t, ok, "width %d: click on 'Y' at col %d must fire", width, x)
		assert.Equalf(t, runtime.ResumeApprove(), resume.Response.Resume, "width %d: 'Y' must approve the single call", width)
	}
}

// Every painted cell, including descriptions, dispatches only its own decision.
func TestToolConfirmationDialog_ActionCellsAndGaps(t *testing.T) {
	for _, width := range []int{20, 30, 45, 80, 120} {
		d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(width, 30)
		seen := map[string]bool{}
		for selected := range 6 {
			d.FocusDefaultAction()
			d.focusedAction = selected
			d.revealChoice()
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			selected, ok := d.SelectedActionKey()
			require.True(t, ok)
			for y := row; y < row+lipgloss.Height(view); y++ {
				for x := col; x < col+lipgloss.Width(view); x++ {
					action, hit := d.ActionKeyAt(x, y, dl)
					if !hit || action.Code != selected.Code {
						continue
					}
					seen[action.Text] = true
					msgs := collectMsgs(clickCell(d, y, x))
					if action.Text == "R" {
						require.True(t, hasMsg[OpenDialogMsg](msgs))
						continue
					}
					response, ok := findMsg[tuimessages.InteractionResponseMsg](msgs)
					require.True(t, ok, "width %d cell %d,%d action %s", width, x, y, action.Text)
					want := map[string]runtime.ResumeRequest{"N": runtime.ResumeReject(""), "Y": runtime.ResumeApprove(), "T": runtime.ResumeApproveTool("shell"), "B": runtime.ResumeApproveBalanced(), "A": runtime.ResumeApproveAutonomous()}
					assert.Equal(t, want[action.Text], response.Response.Resume)
				}
			}
			assert.Nil(t, clickCell(d, row+lipgloss.Height(view), col), "outside clicks never authorize")
		}
		assert.Len(t, seen, 6, "width %d: all decisions stay reachable", width)
	}
}

func TestToolConfirmationDialog_LongPatternPreservesAuthorization(t *testing.T) {
	longWord := "very-long-binary-name-" + strings.Repeat("x", 100)
	event := newConfirmationEvent(nil)
	event.ToolCall.Function.Arguments = `{"cmd":"` + longWord + ` --flag"}`
	d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(40, 12)
	for range 2 {
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Nil(t, cmd)
	}
	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	for y := row; y < row+lipgloss.Height(view); y++ {
		for x := col; x < col+lipgloss.Width(view); x++ {
			key, hit := d.ActionKeyAt(x, y, dl)
			if !hit || key.Text != "T" {
				continue
			}
			_, cmd := d.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			response, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(cmd))
			require.True(t, ok)
			assert.Equal(t, runtime.ResumeApproveTool("shell:cmd="+longWord+"*"), response.Response.Resume)
			return
		}
	}
	t.Fatal("always-allow action was not reachable")
}

// A preempt/custom hook can force a confirmation while the session is
// already autonomous. Choosing Balanced switches the runtime session off
// autonomous, so it must also clear the cached TUI yolo flag — otherwise
// the sidebar keeps saying YOLO and Ctrl+Y "toggles" the session straight
// back to autonomous.
func TestToolConfirmationDialog_BalancedClearsYoloMode(t *testing.T) {
	t.Parallel()

	state := &service.EmbeddedSessionState{}
	state.SetYoloMode(true)
	dialog := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), state)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	_, cmd := dialog.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	require.NotNil(t, cmd)
	assert.False(t, state.YoloMode(), "choosing Balanced must drop the session-wide yolo flag")

	resume, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(cmd))
	require.True(t, ok, "Balanced must dispatch a resume request")
	assert.Equal(t, runtime.ResumeApproveBalanced(), resume.Response.Resume)
}

func TestToolConfirmationDialog_TinyHeightScrollsEveryAction(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(20, 6)
	seen := map[string]bool{}
	for range d.choicesStart + len(d.actionRows) {
		view := d.View()
		row, col := d.Position()
		dl := NewDialogLayout(view, row, col)
		assert.LessOrEqual(t, lipgloss.Height(view), 6)
		for y := row; y < row+lipgloss.Height(view); y++ {
			for x := col; x < col+lipgloss.Width(view); x++ {
				if action, hit := d.ActionKeyAt(x, y, dl); hit {
					seen[action.Text] = true
				}
			}
		}
		_, cmd := d.Update(tea.MouseWheelMsg{X: d.bodyX, Y: d.choicesStart, Button: tea.MouseWheelDown})
		assert.Nil(t, cmd, "wheel movement never authorizes a tool")
	}
	assert.Len(t, seen, 6, "all six actions remain mouse reachable at six rows")
}

func TestToolConfirmationDialogPlainActionsSafeDefaultAndSelectedPolicies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		steps       int
		explanation string
		want        runtime.ResumeRequest
	}{
		{"reject", 0, "Reject this tool call", runtime.ResumeReject("")},
		{"once", 1, "Allow only this tool call", runtime.ResumeApprove()},
		{"tool", 2, "future calls matching shell", runtime.ResumeApproveTool("shell")},
		{"balanced", 3, "classifier-safe calls run automatically", runtime.ResumeApproveBalanced()},
		{"all", 4, "all future tool calls in this session", runtime.ResumeApproveAutonomous()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := newConfirmationEvent(nil)
			event.SessionID = "session"
			event.RequestID = "request"
			d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
			d.SetSize(160, 40)
			initial := ansi.Strip(d.View())
			assert.Contains(t, initial, "› No")
			assert.NotContains(t, initial, "No [N]")
			for _, old := range []string{"Y yes Y", "N no N", "T always allow", "B balanced B", "A all tools A"} {
				assert.NotContains(t, initial, old)
			}
			for range tc.steps {
				_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
			}
			view := d.View()
			assert.NotContains(t, ansi.Strip(view), "↵")
			assert.NotContains(t, strings.Join(strings.Fields(ansi.Strip(view)), " "), tc.explanation)
			assertToolActionsNotUnderlined(t, view)
			footer := strings.Fields(ansi.Strip(d.renderOptions(120)))
			for _, alias := range []string{"Y", "T", "B", "A", "R"} {
				count := 0
				for _, word := range footer {
					if word == "["+alias+"]" {
						count++
					}
				}
				assert.Equal(t, 1, count, "each actual shortcut appears once: %s", alias)
			}
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			msgs := collectMsgs(cmd)
			response, ok := findMsg[tuimessages.InteractionResponseMsg](msgs)
			require.True(t, ok)
			assert.Equal(t, "session", response.SessionID)
			assert.Equal(t, "request", response.Response.InteractionID)
			assert.Equal(t, tc.want, response.Response.Resume)
			_, duplicate := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			assert.Nil(t, duplicate, "resolved dialog cannot emit another decision")
		})
	}
}

func TestToolConfirmationDialogUnknownSafetyPlaceholderDoesNotHideOtherMetadata(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(map[string]string{"safety_label": "unknown", "reason": "approval required", "policy": "team"}), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(120, 30)
	view := ansi.Strip(d.View())
	assert.NotContains(t, view, "safety_label")
	assert.NotContains(t, view, "unknown")
	assert.Contains(t, view, "reason: approval required")
	assert.Contains(t, view, "policy: team")
	for _, classification := range []string{"safe", "destructive"} {
		d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(map[string]string{"safety_label": classification}), &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(120, 30)
		label := classification
		if classification == "safe" {
			label = "read-only"
		}
		assert.Contains(t, ansi.Strip(d.View()), "Safety assessment: recognized "+label+".")
		assert.NotContains(t, ansi.Strip(d.View()), "safety_label")
	}
}

func TestToolConfirmationDialogMnemonicRoutesSettleOnce(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		want runtime.ResumeRequest
	}{
		{'n', runtime.ResumeReject("")},
		{'y', runtime.ResumeApprove()},
		{'t', runtime.ResumeApproveTool("shell")},
		{'b', runtime.ResumeApproveBalanced()},
		{'a', runtime.ResumeApproveAutonomous()},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			event := newConfirmationEvent(nil)
			event.SessionID = "session"
			event.RequestID = "request"
			d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
			d.SetSize(80, 24)
			_, cmd := d.Update(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
			response, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(cmd))
			require.True(t, ok)
			assert.Equal(t, "session", response.SessionID)
			assert.Equal(t, "request", response.Response.InteractionID)
			assert.Equal(t, tc.want, response.Response.Resume)
			_, cmd = d.Update(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
			assert.Nil(t, cmd)
		})
	}
}

func TestToolConfirmationDialogLeftRightTraverseEveryPolicyWithoutApproving(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(30, 12)
	for _, direction := range []rune{tea.KeyRight, tea.KeyLeft} {
		seen := map[rune]bool{}
		for range 6 {
			_, cmd := d.Update(tea.KeyPressMsg{Code: direction})
			assert.Nil(t, cmd, "navigation never authorizes")
			action, ok := d.SelectedActionKey()
			require.True(t, ok)
			seen[action.Code] = true
			view := d.View()
			assert.LessOrEqual(t, lipgloss.Width(view), 30)
			assert.LessOrEqual(t, lipgloss.Height(view), 12)
		}
		assert.Len(t, seen, 6, "all policies remain reachable in either direction")
	}
	assert.False(t, d.responseSent)
}

func TestToolConfirmationDialogReasonCancelReturnsUnansweredNoDefault(t *testing.T) {
	event := newConfirmationEvent(nil)
	event.SessionID = "session"
	event.RequestID = "request"
	parent := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
	parent.SetSize(100, 30)
	_, _ = parent.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	selected, ok := parent.SelectedActionKey()
	require.True(t, ok)
	assert.Equal(t, 'R', selected.Code)
	_, cmd := parent.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	opened, ok := findMsg[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	child := opened.Model.(*multiChoiceDialog)
	child.SetSize(100, 30)
	_, cancel := child.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	result, ok := findMsg[MultiChoiceResultMsg](collectMsgs(cancel))
	require.True(t, ok)
	correlation := result.Context.(tuimessages.InteractionResponseMsg)
	assert.Nil(t, HandleToolRejectionResult(result.Result, correlation))
	assert.False(t, parent.responseSent)
	selected, ok = parent.SelectedActionKey()
	require.True(t, ok)
	assert.Equal(t, 'N', selected.Code)
	assert.Contains(t, ansi.Strip(parent.View()), "› No")
	_, cmd = parent.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	response, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, runtime.ResumeReject(""), response.Response.Resume)
}

func TestToolConfirmationDialogReasonPreservesTextAndCorrelationOnce(t *testing.T) {
	event := newConfirmationEvent(nil)
	event.SessionID = "session"
	event.RequestID = "request"
	parent := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
	parent.SetSize(100, 30)
	_, cmd := parent.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	opened, ok := findMsg[OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	child := opened.Model.(*multiChoiceDialog)
	child.SetSize(100, 30)
	_, _ = child.Update(tea.PasteMsg{Content: "Reject because this needs review"})
	_, cmd = child.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	result, ok := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, ok)
	response := HandleToolRejectionResult(result.Result, result.Context.(tuimessages.InteractionResponseMsg))
	require.NotNil(t, response)
	assert.Equal(t, "session", response.SessionID)
	assert.Equal(t, "request", response.Response.InteractionID)
	assert.Equal(t, runtime.ResumeReject("Reject because this needs review"), response.Response.Resume)
	assert.False(t, hasMsg[tuimessages.InteractionResponseMsg](msgs), "child sends result for root routing, not a second direct runtime response")
	_, cmd = child.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
}

func TestToolConfirmationDialogSpawnPreviewIsStaticBeforeApproval(t *testing.T) {
	event := newConfirmationEvent(nil)
	event.ToolCall = tools.ToolCall{ID: "spawn", Function: tools.FunctionCall{Name: "spawn_subagent", Arguments: `{"agent":"reviewer","task":"Inspect **only** the proposed files","expected_output":"A review","extra":9007199254740993}`}}
	event.ToolDefinition = tools.Tool{Name: "spawn_subagent"}
	d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(100, 40)
	assert.Nil(t, d.Init(), "preview must not start execution animations")
	view := d.View()
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "Proposed:")
	assert.Contains(t, plain, "reviewer")
	assert.Contains(t, plain, "Inspect")
	assert.Contains(t, plain, "9007199254740993")
	assert.NotContains(t, plain, "Spawning")
	assert.NotContains(t, plain, "Running")
	for range 5 {
		assert.Equal(t, view, d.View())
	}
	_, cmd := d.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	assert.Nil(t, cmd)
	assert.False(t, d.responseSent)
}

func assertToolActionsNotUnderlined(t *testing.T, view string) {
	t.Helper()
	for _, sequence := range regexp.MustCompile(`\x1b\[([0-9;:]*)m`).FindAllStringSubmatch(view, -1) {
		params := strings.Split(sequence[1], ";")
		for i := 0; i < len(params); i++ {
			parameter := params[i]
			if (parameter == "38" || parameter == "48" || parameter == "58") && i+1 < len(params) {
				switch params[i+1] {
				case "2":
					i += 4
				case "5":
					i += 2
				}
				continue
			}
			assert.NotEqual(t, "4", parameter, "underline SGR must not be enabled")
			assert.NotEqual(t, "21", parameter, "double underline SGR must not be enabled")
			assert.False(t, strings.HasPrefix(parameter, "4:"), "underline variants must not be enabled")
		}
	}
}

func TestToolConfirmationDialogVerticalDecisionsAndBracketedShortcuts(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(165, 47)
	view := ansi.Strip(d.View())
	labels := []string{"› No", "Yes, once", "Always allow tool", "Balanced mode", "Allow all tools", "Reject with reason"}
	lastRow := -1
	for _, label := range labels {
		y, _, found := locateInView(d, label)
		require.True(t, found, "full choice label must be visible: %s", label)
		assert.Greater(t, y, lastRow, "choices must occupy distinct vertical rows")
		lastRow = y
	}
	assert.NotContains(t, view, "↑/↓ choose")
	assert.NotContains(t, view, "Enter confirm")
	assert.NotContains(t, view, "shortcut/click applies")
	assert.NotContains(t, view, "Esc")
	assert.NotContains(t, view, "Close")
}

func TestToolConfirmationDialogVerticalNavigationThenDismissDeniesOnce(t *testing.T) {
	for _, closeControl := range []bool{false, true} {
		event := newConfirmationEvent(nil)
		event.SessionID, event.RequestID = "session", "request"
		d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(30, 12)
		for range 4 {
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			assert.Nil(t, cmd)
		}
		selected, ok := d.SelectedActionKey()
		require.True(t, ok)
		assert.Equal(t, 'A', selected.Code)
		var cmd tea.Cmd
		if closeControl {
			view := d.View()
			row, col := d.Position()
			x, y, ok := closeControlCell(lipgloss.Width(view), lipgloss.Height(view))
			require.True(t, ok)
			_, cmd = d.Update(tea.MouseClickMsg{X: col + x, Y: row + y, Button: tea.MouseLeft})
		} else {
			_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		}
		msgs := collectMsgs(cmd)
		response, ok := findMsg[tuimessages.InteractionResponseMsg](msgs)
		require.True(t, ok)
		assert.Equal(t, "session", response.SessionID)
		assert.Equal(t, "request", response.Response.InteractionID)
		assert.Equal(t, runtime.ResumeReject(""), response.Response.Resume)
		assert.True(t, hasMsg[CloseDialogMsg](msgs))
		assert.Nil(t, d.CancelDialogCmd())
		_, duplicate := d.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		assert.Nil(t, duplicate)
	}
}

func TestToolConfirmationDialogChoicesHaveAlignedHintsWithoutDescriptions(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(165, 47)
	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	shortcutX := -1
	for _, action := range d.actions {
		y, x, found := locateInView(d, action.Label)
		require.True(t, found)
		key, hit := d.ActionKeyAt(x, y, dl)
		require.True(t, hit)
		assert.Equal(t, action.Key, key)
		if shortcut := action.shortcut(); shortcut != "" {
			hy, hx, found := locateInView(d, "["+shortcut+"]")
			require.True(t, found)
			assert.Equal(t, y, hy)
			assert.GreaterOrEqual(t, hx-x-lipgloss.Width(action.Label), 2)
			if shortcutX >= 0 {
				assert.Equal(t, shortcutX, hx)
			}
			shortcutX = hx
		}
	}
	questionY, _, found := locateInView(d, "Do you want to allow this tool call?")
	require.True(t, found)
	assert.Equal(t, questionY+2, d.choicesStart, "one blank row separates the question and choices")
	assertToolConfirmationNoRoutineHints(t, view)
}

func assertToolConfirmationNoRoutineHints(t *testing.T, view string) {
	t.Helper()
	plain := ansi.Strip(view)
	for _, removed := range []string{
		"Reject this tool call without running it.", "Allow only this tool call.",
		"future calls matching", "classifier-safe calls run automatically",
		"all future tool calls in this session", "Choose or write a rejection reason",
		"↑/↓", "↑↓", "Enter confirm", "shortcut/click applies", "PgUp", "PgDn", "↵",
	} {
		assert.NotContains(t, plain, removed)
	}
}

func TestToolConfirmationDialogOverflowKeepsPreviewAndChoices(t *testing.T) {
	event := newConfirmationEvent(nil)
	event.ToolCall.Function.Arguments = `{"cmd":"` + strings.Repeat("long proposed command ", 100) + `"}`
	for _, size := range [][2]int{{165, 30}, {80, 24}, {80, 16}, {30, 8}, {20, 6}} {
		d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(size[0], size[1])
		initial := ansi.Strip(d.View())
		assert.Contains(t, initial, "Proposed:", "initial view prioritizes proposed inputs")
		assert.Zero(t, d.BodyScrollOffset())
		action, ok := d.SelectedActionKey()
		require.True(t, ok)
		assert.Equal(t, 'N', action.Code)
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		assert.Nil(t, cmd)
		assert.Zero(t, d.BodyScrollOffset(), "choosing never scrolls proposed arguments")
		for range 6 {
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			y, x, found := locateInView(d, "›")
			require.True(t, found, "selected label is visible even in a tiny viewport")
			key, hit := d.ActionKeyAt(x, y, dl)
			require.True(t, hit)
			selected, ok := d.SelectedActionKey()
			require.True(t, ok)
			assert.Equal(t, selected, key)
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assertToolConfirmationNoRoutineHints(t, view)
			_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			assert.Nil(t, cmd)
		}
		assert.False(t, d.responseSent)
	}
}

func TestToolConfirmationDialogResizeRevealsSelectedBlock(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(165, 47)
	for range 4 {
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.Nil(t, cmd)
	}
	for _, size := range [][2]int{{50, 12}, {20, 6}, {165, 47}} {
		d.SetSize(size[0], size[1])
		y, x, found := locateInView(d, "›")
		require.True(t, found)
		view := d.View()
		row, col := d.Position()
		key, hit := d.ActionKeyAt(x, y, NewDialogLayout(view, row, col))
		require.True(t, hit)
		assert.Equal(t, 'A', key.Code)
	}
}

func TestToolConfirmationIndependentDecisionRegion(t *testing.T) {
	event := newConfirmationEvent(nil)
	event.ToolCall.Function.Arguments = `{"cmd":"` + strings.Repeat("界 é long argument ", 120) + `"}`
	for _, size := range [][2]int{{120, 35}, {80, 24}, {40, 10}, {20, 6}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
			d.SetSize(size[0], size[1])
			view := d.View()
			t.Logf("%dx%d\n%s", size[0], size[1], ansi.Strip(view))
			require.LessOrEqual(t, lipgloss.Height(view), size[1])
			require.LessOrEqual(t, lipgloss.Width(view), size[0])
			require.Contains(t, ansi.Strip(view), "› No")
			require.NotContains(t, ansi.Strip(view), "Esc")
			if size[1] >= 24 {
				for _, choice := range d.options() {
					require.Contains(t, ansi.Strip(view), choice.Label)
				}
				require.GreaterOrEqual(t, d.bodyHeight, 3, "arguments retain their own usable viewport")
			}
			choiceOffset := d.choiceScroll.ScrollOffset()
			d.Update(tea.MouseWheelMsg{X: d.bodyX, Y: d.bodyY, Button: tea.MouseWheelDown})
			require.Positive(t, d.BodyScrollOffset())
			require.Equal(t, choiceOffset, d.choiceScroll.ScrollOffset())
			bodyOffset := d.BodyScrollOffset()
			d.Update(tea.MouseWheelMsg{X: d.bodyX, Y: d.choicesStart, Button: tea.MouseWheelDown})
			require.Equal(t, bodyOffset, d.BodyScrollOffset(), "choice wheel cannot move arguments")
			for _, code := range []rune{tea.KeyPgDown, tea.KeyPgUp} {
				_, cmd := d.Update(tea.KeyPressMsg{Code: code})
				require.Nil(t, cmd)
				require.Equal(t, bodyOffset, d.BodyScrollOffset(), "page keys do not scroll tool inputs")
			}
			for range 6 {
				oldHeight := lipgloss.Height(d.View())
				d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				require.Equal(t, oldHeight, lipgloss.Height(d.View()), "selection preserves stable height")
				require.Contains(t, ansi.Strip(d.View()), "›")
			}
			require.False(t, d.responseSent)
		})
	}
}

func TestToolConfirmationChoiceRegionIsStableAndBounded(t *testing.T) {
	d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(80, 24)
	initialHeight := lipgloss.Height(d.View())
	for range 6 {
		view := d.View()
		row, col := d.Position()
		dl := NewDialogLayout(view, row, col)
		require.Equal(t, initialHeight, lipgloss.Height(view))
		for y := d.choicesStart + d.choiceHeight; y < row+dl.Height; y++ {
			_, hit := d.ActionKeyAt(d.bodyX, y, dl)
			require.False(t, hit, "space below choices cannot authorize")
		}
		assertToolConfirmationNoRoutineHints(t, view)
		d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
}

func TestToolConfirmationResizeKeepsDecisionPendingAndPrepared(t *testing.T) {
	r := newDialogRuntime()
	mgr := &manager{runtime: r, width: 80, height: 24}
	d := NewToolConfirmationDialog(r, newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	advanceResize(mgr, dialogOpenDuration)
	mgr.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	preparations := d.bodyPreparationCount
	for mgr.stack[0].anim.Running() {
		mgr.Update(tea.MouseClickMsg{X: d.bodyX, Y: d.choicesStart, Button: tea.MouseLeft})
		require.False(t, d.responseSent, "transient hitboxes cannot answer")
		mgr.handleTick(acceptedDialogTick(r, r.Continue()))
		mgr.GetLayerInfos()
		require.Equal(t, preparations, d.bodyPreparationCount, "animation ticks do not reprepare arguments or choices")
	}
	selected, ok := d.SelectedActionKey()
	require.True(t, ok)
	require.Equal(t, 'N', selected.Code)
	mgr.Cleanup()
	require.Zero(t, r.ActiveCount())
}
