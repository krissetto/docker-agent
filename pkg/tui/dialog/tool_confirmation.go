package dialog

import (
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const toolConfirmDialogWidthPercent = 70

// ConfirmationSessionState is the session-state surface the confirmation
// dialog needs: the message list's surface for rendering the tool call, plus
// the session-wide approval flip for the "all tools" decision.
type ConfirmationSessionState interface {
	messages.SessionState
	SetYoloMode(yoloMode bool)
}

var (
	_ ConfirmationSessionState = (*service.SessionState)(nil)
	_ ConfirmationSessionState = (*service.EmbeddedSessionState)(nil)
)

type toolConfirmationDialog struct {
	BaseDialog

	msg               *runtime.ToolCallConfirmationEvent
	keyMap            toolconfirm.KeyMap
	sessionState      ConfirmationSessionState
	scrollView        messages.Model
	permissionPattern string // cached permission pattern for this tool call
}

func (d *toolConfirmationDialog) dialogDimensions() (dialogWidth, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(toolConfirmDialogWidthPercent, 36, 120)
	contentWidth = max(1, dialogWidth-styles.DialogStyle.GetHorizontalFrameSize())
	return dialogWidth, contentWidth
}

func (d *toolConfirmationDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	dialogWidth, _ := d.dialogDimensions()
	bodyWidth := d.BodyContentWidth(dialogWidth)
	d.scrollView.SetSize(bodyWidth, max(1, height))
	d.scrollView.SetSize(bodyWidth, max(1, d.scrollView.RenderedContentHeight()))
	d.prepareLayout()
	return nil
}

func (d *toolConfirmationDialog) renderOptions(contentWidth int) string {
	bindings := toolconfirm.OptionsHelp(d.permissionPattern)
	actions := make([]Action, 0, len(bindings)/2)
	for i := 0; i+1 < len(bindings); i += 2 {
		action, _ := utf8.DecodeRuneInString(bindings[i])
		actions = append(actions, Action{Label: bindings[i] + " " + bindings[i+1], Key: tea.KeyPressMsg{Code: action, Text: bindings[i]}})
	}
	return d.RenderActions(contentWidth, actions...)
}

// safetyConventionKeys are the metadata keys the safer_shell builtin
// uses to surface its verdict to the UI when paired with a
// `blast_radius` key. The renderer composes a user-facing warning
// from these instead of showing raw key/value pairs (avoid leaking
// implementation details into the prompt).
//
// The convention only applies when `blast_radius` is also present —
// `category` and `reason` are deliberately generic key names that a
// permission_request hook might use for unrelated purposes, so we
// keep them rendering as plain text when no blast radius indicates
// a safety verdict is in play.
var safetyConventionKeys = map[string]struct{}{
	"blast_radius": {},
	"category":     {},
	"reason":       {},
	"safety_label": {},
}

// blastRadiusBadge maps the safer_shell builtin's blast_radius
// vocabulary onto theme colors. Unknown values render unstyled so the
// renderer never silently drops data it doesn't recognise.
func blastRadiusBadge(value string) string {
	style := styles.BaseStyle.Bold(true)
	switch value {
	case "safe":
		style = style.Foreground(styles.Success)
	case "low":
		style = style.Foreground(styles.Success)
	case "medium":
		style = style.Foreground(styles.Warning)
	case "high":
		style = style.Foreground(styles.Error)
	case "unknown":
		style = style.Foreground(styles.TextMuted)
	default:
		return value
	}
	return style.Render(value)
}

// renderSafetyWarning composes the classifier's verdict block from
// the safer_shell metadata. Safe verdicts render a reassuring
// heading; destructive / unknown verdicts render a warning. Returns
// "" when no blast_radius is present.
func (d *toolConfirmationDialog) renderSafetyWarning(contentWidth int) string {
	radius, ok := d.msg.Metadata["blast_radius"]
	if !ok {
		return ""
	}

	var heading string
	switch radius {
	case "safe":
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Success).
			Render("✓ Read-only command — " + blastRadiusBadge(radius))
	case "unknown":
		// Not positively recognised ≠ destructive: crying wolf on every
		// unlisted command would teach users to ignore the real warnings.
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Warning).
			Render("⚠  Unrecognised command — not classified as safe")
	default:
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Warning).
			Render(fmt.Sprintf("⚠  Destructive command — %s blast radius", blastRadiusBadge(radius)))
	}

	lines := []string{heading}
	if reason := d.msg.Metadata["reason"]; reason != "" {
		lines = append(lines, "  "+styles.DialogContentStyle.Render(reason))
	}

	return styles.DialogContentStyle.Width(contentWidth).Render(
		lipgloss.JoinVertical(lipgloss.Left, lines...),
	)
}

// renderMetadata renders the key/value annotations attached to the
// confirmation prompt (static toolset metadata merged with any
// permission_request / preempt-yolo pre_tool_use hook contributions). Returns ""
// when there is none.
//
// Metadata keys the safer_shell builtin uses to express its verdict
// (see [safetyConventionKeys]) are excluded — they're rendered by
// [renderSafetyWarning] as a polished warning block instead of as
// raw key/value rows. Anything else renders as plain text so a
// permission_request hook's freeform annotations still surface.
func (d *toolConfirmationDialog) renderMetadata(contentWidth int) string {
	if len(d.msg.Metadata) == 0 {
		return ""
	}

	// Only suppress convention keys when blast_radius is present —
	// then renderSafetyWarning is composing the message from them.
	// Otherwise (no safety verdict in play), keys like `reason` and
	// `category` are just regular permission_request metadata and
	// should render as plain pairs.
	_, hasBlastRadius := d.msg.Metadata["blast_radius"]

	var lines []string
	for _, k := range slices.Sorted(maps.Keys(d.msg.Metadata)) {
		if hasBlastRadius {
			if _, ok := safetyConventionKeys[k]; ok {
				continue
			}
		}
		key := styles.MutedStyle.Render(k + ": ")
		val := styles.DialogContentStyle.Render(d.msg.Metadata[k])
		lines = append(lines, fmt.Sprintf("  %s%s", key, val))
	}
	if len(lines) == 0 {
		return ""
	}

	header := styles.SecondaryStyle.Render("Metadata")
	lines = append([]string{header}, lines...)
	return styles.DialogContentStyle.Width(contentWidth).Render(
		lipgloss.JoinVertical(lipgloss.Left, lines...),
	)
}

// NewToolConfirmationDialog creates a new tool confirmation dialog
func NewToolConfirmationDialog(ar *animation.Runtime, msg *runtime.ToolCallConfirmationEvent, sessionState ConfirmationSessionState) Dialog {
	// Create scrollable view with minimal initial size (will be updated in SetSize)
	scrollView := messages.NewScrollableView(ar, 1, 1, sessionState)

	// Add the tool call message to the view
	scrollView.AddOrUpdateToolCall(
		"", // agentName - empty for dialog context
		msg.ToolCall,
		msg.ToolDefinition,
		types.ToolStatusConfirmation,
	)

	// Build and cache the permission pattern for display and use
	pattern := toolconfirm.BuildPermissionPattern(msg.ToolCall)

	return &toolConfirmationDialog{
		msg:               msg,
		sessionState:      sessionState,
		keyMap:            toolconfirm.DefaultKeyMap(),
		scrollView:        scrollView,
		permissionPattern: pattern,
	}
}

// Init initializes the tool confirmation dialog
func (d *toolConfirmationDialog) Init() tea.Cmd {
	return d.scrollView.Init()
}

func (d *toolConfirmationDialog) response(request runtime.ResumeRequest) tuimessages.InteractionResponseMsg {
	return tuimessages.InteractionResponseMsg{
		SessionID: d.msg.SessionID,
		Response: runtime.InteractionResponse{
			InteractionID: d.msg.RequestID,
			Kind:          runtime.InteractionConfirmation,
			Resume:        request,
		},
	}
}

// OutsideClickDismissCmd keeps this mandatory decision open on outside clicks.
func (d *toolConfirmationDialog) OutsideClickDismissCmd() tea.Cmd { return nil }

// CancelDialogCmd implements SemanticCloser for every non-affirmative shared
// dismissal path (outside click, close control, Escape, and Ctrl-C).
func (d *toolConfirmationDialog) CancelDialogCmd() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(d.response(toolconfirm.Reject.Resume("", ""))),
	)
}

// executeAction dispatches a confirmation decision.
func (d *toolConfirmationDialog) executeAction(decision toolconfirm.Decision) (layout.Model, tea.Cmd) {
	if decision != toolconfirm.Reject && !d.claimResponse() {
		return d, nil
	}
	switch decision {
	case toolconfirm.Approve:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(d.response(toolconfirm.Approve.Resume("", ""))),
		)
	case toolconfirm.Reject:
		return d, core.CmdHandler(OpenDialogMsg{
			Model: NewToolRejectionReasonDialog(d.msg.SessionID, d.msg.RequestID),
		})
	case toolconfirm.ApproveTool:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(d.response(toolconfirm.ApproveTool.Resume(d.permissionPattern, ""))),
		)
	case toolconfirm.ApproveBalanced:
		// A preempt/custom hook can raise a confirmation while the session
		// is autonomous. Balanced moves the runtime session off autonomous,
		// so the cached TUI flag must drop too — otherwise the sidebar keeps
		// claiming YOLO and Ctrl+Y "toggles" it straight back to autonomous.
		// Mirrors ApproveSession setting it true below.
		d.sessionState.SetYoloMode(false)
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(d.response(toolconfirm.ApproveBalanced.Resume("", ""))),
		)
	case toolconfirm.ApproveSession:
		d.sessionState.SetYoloMode(true)
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(d.response(toolconfirm.ApproveSession.Resume("", ""))),
		)
	}
	return d, nil
}

// Update handles messages for the tool confirmation dialog
func (d *toolConfirmationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.MouseClickMsg:
		defer d.prepareLayout()
	}
	if handled, cmd := d.UpdateBodyScroll(msg); handled {
		return d, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg)
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}

		if decision, ok := d.keyMap.DecisionFor(msg); ok {
			return d.executeAction(decision)
		}

		// Forward scrolling keys to the scroll view
		if _, isScrollKey := core.GetScrollDirection(msg); isScrollKey {
			updatedScrollView, cmd := d.scrollView.Update(msg)
			d.scrollView = updatedScrollView.(messages.Model)
			return d, cmd
		}

	case tuimessages.WheelCoalescedMsg:
		updatedScrollView, cmd := d.scrollView.Update(msg)
		d.scrollView = updatedScrollView.(messages.Model)
		return d, cmd
	}

	return d, nil
}

func (d *toolConfirmationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	view := d.View()
	row, col := d.CenterDialog(view)
	if action, ok := d.ActionKeyAt(msg.X, msg.Y, NewDialogLayout(view, row, col)); ok {
		return d.Update(action)
	}
	return d, nil
}

func (d *toolConfirmationDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth, contentWidth := d.dialogDimensions()
	bodyWidth := d.BodyContentWidth(dialogWidth)
	header = RenderTitle(toolconfirm.Title, contentWidth, styles.DialogTitleStyle)
	parts := []string{d.scrollView.View()}
	if warning := d.renderSafetyWarning(bodyWidth); warning != "" {
		parts = append(parts, "", warning)
	}
	if metadata := d.renderMetadata(bodyWidth); metadata != "" {
		parts = append(parts, "", metadata)
	}
	parts = append(parts, "", styles.DialogQuestionStyle.Width(bodyWidth).Render(toolconfirm.Question))
	return styles.DialogStyle, dialogWidth, header, lipgloss.JoinVertical(lipgloss.Left, parts...), d.renderOptions(contentWidth)
}

func (d *toolConfirmationDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *toolConfirmationDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

func (d *toolConfirmationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// DialogClosable reports that this mandatory decision has no generic close chrome.
func (d *toolConfirmationDialog) DialogClosable() bool { return false }

// StopAnimations releases subscriptions without changing the pending decision.
func (d *toolConfirmationDialog) StopAnimations() {
	if d.scrollView != nil {
		d.scrollView.StopAnimations()
	}
}

func (d *toolConfirmationDialog) Cleanup() { d.StopAnimations() }
