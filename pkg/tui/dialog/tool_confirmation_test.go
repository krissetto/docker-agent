package dialog

import (
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
	assert.Contains(t, strings.Join(strings.Fields(plain), " "), "shell ls")
	assert.Equal(t, 1, dialog.(*toolConfirmationDialog).scrollView.RenderedContentHeight(),
		"decoded shell ls renders one intrinsic tool row")
	assert.LessOrEqual(t, lipgloss.Height(view), 12,
		"one-row content uses content height plus fixed prompt chrome, not the 80% viewport cap")
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
// command UX: when the confirmation event carries the safer_shell
// builtin's `blast_radius` metadata, the dialog composes a polished
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
// metadata alongside safer_shell's verdict. The warning block uses
// the safer_shell convention keys, and the extra keys still render
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

		y, x, found := locateInView(d, "Y yes")
		require.Truef(t, found, "width %d: 'Y yes' must be visible on some options row", width)

		resume, ok := findMsg[tuimessages.InteractionResponseMsg](collectMsgs(clickCell(d, y, x)))
		require.Truef(t, ok, "width %d: click on 'Y' at col %d must fire", width, x)
		assert.Equalf(t, runtime.ResumeApprove(), resume.Response.Resume, "width %d: 'Y' must approve the single call", width)
	}
}

// Every painted cell of every decision pill must dispatch only that decision.
func TestToolConfirmationDialog_ActionCellsAndGaps(t *testing.T) {
	for _, width := range []int{20, 30, 45, 80, 120} {
		d := NewToolConfirmationDialog(animation.NewRuntime(), newConfirmationEvent(nil), &service.SessionState{}).(*toolConfirmationDialog)
		d.SetSize(width, 30)
		view := d.View()
		row, col := d.Position()
		dl := NewDialogLayout(view, row, col)
		seen := map[string]bool{}
		for y := row; y < row+lipgloss.Height(view); y++ {
			for x := col; x < col+lipgloss.Width(view); x++ {
				action, hit := d.ActionKeyAt(x, y, dl)
				if !hit {
					continue
				}
				seen[action.Text] = true
				msgs := collectMsgs(clickCell(d, y, x))
				if action.Text == "N" {
					require.True(t, hasMsg[OpenDialogMsg](msgs))
					continue
				}
				response, ok := findMsg[tuimessages.InteractionResponseMsg](msgs)
				require.True(t, ok, "width %d cell %d,%d action %s", width, x, y, action.Text)
				want := map[string]runtime.ResumeRequest{"Y": runtime.ResumeApprove(), "T": runtime.ResumeApproveTool("shell"), "B": runtime.ResumeApproveBalanced(), "A": runtime.ResumeApproveAutonomous()}
				assert.Equal(t, want[action.Text], response.Response.Resume)
			}
		}
		assert.Len(t, seen, 5, "width %d: all decisions stay reachable", width)
		assert.Nil(t, clickCell(d, row+lipgloss.Height(view), col), "outside clicks never authorize")
	}
}

func TestToolConfirmationDialog_LongPatternPreservesAuthorization(t *testing.T) {
	longWord := "very-long-binary-name-" + strings.Repeat("x", 100)
	event := newConfirmationEvent(nil)
	event.ToolCall.Function.Arguments = `{"cmd":"` + longWord + ` --flag"}`
	d := NewToolConfirmationDialog(animation.NewRuntime(), event, &service.SessionState{}).(*toolConfirmationDialog)
	d.SetSize(40, 12)
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
	for range 12 {
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
		_, cmd := d.Update(tea.MouseWheelMsg{X: col + 3, Y: row + lipgloss.Height(view) - 2, Button: tea.MouseWheelDown})
		assert.Nil(t, cmd, "wheel movement never authorizes a tool")
	}
	assert.Len(t, seen, 5, "all five decisions remain mouse reachable at six rows")
}
