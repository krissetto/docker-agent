package dialog

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
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
	permissionPattern string // cached permission pattern for this tool call
	choicesStart      int
}

func (d *toolConfirmationDialog) dialogDimensions() (dialogWidth, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(toolConfirmDialogWidthPercent, 36, 120)
	contentWidth = max(1, dialogWidth-styles.DialogStyle.GetHorizontalFrameSize())
	return dialogWidth, contentWidth
}

func (d *toolConfirmationDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	if d.ActionsFocused() {
		d.revealChoice()
	}
	return nil
}

func (d *toolConfirmationDialog) renderOptions(contentWidth int) string {
	return d.RenderChoices(contentWidth,
		Action{Label: "No", Description: "Reject this tool call without running it.", Key: tea.KeyPressMsg{Code: 'N', Text: "N"}, Default: true, HideShortcut: true},
		Action{Label: "Yes, once", Description: "Allow only this tool call. Future calls still require permission.", Key: tea.KeyPressMsg{Code: 'Y', Text: "Y"}},
		Action{Label: "Always allow tool", Description: "Allow this call and future calls matching " + d.permissionPattern + ".", Key: tea.KeyPressMsg{Code: 'T', Text: "T"}},
		Action{Label: "Balanced mode", Description: "Allow this call and switch this session to Balanced mode: classifier-safe calls run automatically; other calls still require permission.", Key: tea.KeyPressMsg{Code: 'B', Text: "B"}},
		Action{Label: "Allow all tools", Description: "Allow this call and all future tool calls in this session without confirmation.", Key: tea.KeyPressMsg{Code: 'A', Text: "A"}},
		Action{Label: "Reject with reason", Description: "Choose or write a rejection reason before rejecting this call. Escape returns here without answering.", Key: tea.KeyPressMsg{Code: 'R', Text: "R"}},
	)
}

func (d *toolConfirmationDialog) renderNavigation(contentWidth int) string {
	help := "↑/↓ choose · Enter confirm · shortcut/click applies · wheel scroll · Esc denies"
	if contentWidth < 80 {
		help = "↑/↓ choose · Enter confirm · Esc denies"
	}
	if contentWidth < 40 || d.height < 10 {
		help = ansi.Truncate("↑↓ · ↵ · Esc", contentWidth, "")
	}
	return styles.MutedStyle.Width(contentWidth).Align(lipgloss.Left).Render(help)
}

// safetyConventionKeys group a blast-radius assessment into a readable warning.
// Without blast_radius, generic reason/category fields remain hook annotations.
var safetyConventionKeys = map[string]struct{}{
	"blast_radius": {},
	"category":     {},
	"reason":       {},
	"safety_label": {},
}

// blastRadiusBadge maps the assessment's blast_radius vocabulary onto theme colors.
// Unknown values render unstyled so the
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

// renderSafetyWarning describes the supplied assessment; classification is not authorization.
// Native tool labeling and hook metadata may contribute these fields.
func (d *toolConfirmationDialog) renderSafetyWarning(contentWidth int) string {
	radius, ok := d.msg.Metadata["blast_radius"]
	if !ok {
		switch d.msg.Metadata["safety_label"] {
		case "safe":
			return styles.DialogContentStyle.Foreground(styles.Success).Width(contentWidth).Render("Safety assessment: recognized read-only.")
		case "destructive":
			return styles.DialogContentStyle.Foreground(styles.Warning).Width(contentWidth).Render("Safety assessment: recognized destructive.")
		default:
			return ""
		}
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
// Assessment fields (see [safetyConventionKeys]) are rendered by
// [renderSafetyWarning] as a readable warning block instead of as
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
		if k == "safety_label" {
			switch strings.ToLower(strings.TrimSpace(d.msg.Metadata[k])) {
			case "", "unknown", "safe", "destructive":
				continue
			}
		}
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
func NewToolConfirmationDialog(_ *animation.Runtime, msg *runtime.ToolCallConfirmationEvent, sessionState ConfirmationSessionState) Dialog {
	// Build and cache the permission pattern for display and use
	pattern := toolconfirm.BuildPermissionPattern(msg.ToolCall)

	return &toolConfirmationDialog{
		msg:               msg,
		sessionState:      sessionState,
		keyMap:            toolconfirm.DefaultKeyMap(),
		permissionPattern: pattern,
	}
}

// Init initializes the tool confirmation dialog
func (d *toolConfirmationDialog) Init() tea.Cmd { return nil }

// InteractionIdentity identifies the runtime decision answered by this dialog.
func (d *toolConfirmationDialog) InteractionIdentity() (sessionID, requestID string) {
	return d.msg.SessionID, d.msg.RequestID
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
	if decision == toolconfirm.Reject {
		cmd := d.CancelDialogCmd()
		return d, cmd
	}
	if !d.claimResponse() {
		return d, nil
	}
	switch decision {
	case toolconfirm.Approve:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(d.response(toolconfirm.Approve.Resume("", ""))),
		)
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
	revealSelection := false
	switch msg.(type) {
	case tea.KeyPressMsg, tea.MouseClickMsg:
		defer func() {
			d.prepareLayout()
			if revealSelection {
				d.revealChoice()
			}
		}()
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
		if !d.ActionsFocused() {
			switch msg.Code {
			case tea.KeyLeft, tea.KeyRight, tea.KeyUp, tea.KeyDown, tea.KeyEnter:
				d.FocusDefaultAction()
			}
		}
		if action, handled := d.HandleActionKey(msg); handled {
			if action.Code == 0 {
				revealSelection = d.ActionsFocused()
				return d, nil
			}
			msg = action
		}
		if cmd := HandleQuit(msg); cmd != nil || msg.Code == tea.KeyEscape {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}

		if (msg.String() == "r" || msg.String() == "R") && !d.responseSent {
			d.FocusDefaultAction()
			d.BlurActions()
			return d, core.CmdHandler(OpenDialogMsg{Model: NewToolRejectionReasonDialog(d.msg.SessionID, d.msg.RequestID)})
		}

		if decision, ok := d.keyMap.DecisionFor(msg); ok {
			return d.executeAction(decision)
		}
	}

	return d, nil
}

func (d *toolConfirmationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	view := d.View()
	row, col := d.CenterDialog(view)
	dl := NewDialogLayout(view, row, col)
	if d.CloseButtonHit(msg, dl) {
		cmd := d.CancelDialogCmd()
		return d, cmd
	}
	if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
		d.BlurActions()
		return d.Update(action)
	}
	return d, nil
}

func (d *toolConfirmationDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth, contentWidth := d.dialogDimensions()
	bodyWidth := d.BodyContentWidth(dialogWidth)
	header = RenderTitle(toolconfirm.Title, contentWidth, styles.DialogTitleStyle)
	footer = d.renderNavigation(contentWidth)
	var parts []string
	if arguments := toolconfirm.Preview(d.msg.ToolCall, d.msg.ToolDefinition, bodyWidth); arguments != "" {
		parts = append(parts, arguments)
	}
	if warning := d.renderSafetyWarning(bodyWidth); warning != "" {
		if len(parts) > 0 {
			parts = append(parts, "")
		}
		parts = append(parts, warning)
	}
	if metadata := d.renderMetadata(bodyWidth); metadata != "" {
		if len(parts) > 0 {
			parts = append(parts, "")
		}
		parts = append(parts, metadata)
	}
	if len(parts) > 0 {
		parts = append(parts, "")
	}
	parts = append(parts, styles.DialogQuestionStyle.Width(bodyWidth).Align(lipgloss.Left).Render(toolconfirm.Question), "")
	d.choicesStart = lipgloss.Height(lipgloss.JoinVertical(lipgloss.Left, parts...))
	parts = append(parts, d.renderOptions(bodyWidth))
	return styles.DialogStyle, dialogWidth, header, lipgloss.JoinVertical(lipgloss.Left, parts...), footer
}

func (d *toolConfirmationDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *toolConfirmationDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

// Choices live in the body viewport; the fixed footer contains only navigation help.
func (d *toolConfirmationDialog) ActionKeyAt(x, y int, dl DialogLayout) (tea.KeyPressMsg, bool) {
	bodyX, bodyY, width, height := d.BodyScrollBounds()
	if x < bodyX || x >= bodyX+width || y < bodyY || y >= bodyY+height || y < dl.Row || y >= dl.Row+dl.Height {
		return tea.KeyPressMsg{}, false
	}
	row := y - bodyY + d.BodyScrollOffset() - d.choicesStart
	if row >= 0 && row < len(d.actionRows) {
		for _, hit := range d.actionRows[row].hits {
			if x-bodyX >= hit.x && x-bodyX < hit.x+hit.width {
				return hit.key, true
			}
		}
	}
	return tea.KeyPressMsg{}, false
}

func (d *toolConfirmationDialog) revealChoice() {
	selected := d.selectedAction(d.actions)
	if selected < 0 || selected >= len(d.actionLines) {
		return
	}
	end := len(d.actionRows) - 1
	if selected+1 < len(d.actionLines) {
		end = d.actionLines[selected+1] - 2
	}
	// Reveal the description when it fits, but always keep the choice label visible.
	d.bodyScroll.EnsureRangeVisible(d.choicesStart+d.actionLines[selected], d.choicesStart+end)
}

func (d *toolConfirmationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// DialogClosable allows explicit dismissal through the correlated rejection path.
func (d *toolConfirmationDialog) DialogClosable() bool { return true }

// StopAnimations has no work: proposed-call previews never own execution animations.
func (d *toolConfirmationDialog) StopAnimations() {}

func (d *toolConfirmationDialog) Cleanup() {}
