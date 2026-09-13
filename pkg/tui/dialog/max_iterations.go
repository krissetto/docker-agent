//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Layout constants for max iterations dialog.
const (
	maxIterDialogWidthPercent = 60 // Dialog width as percentage of screen
	maxIterDialogMinWidth     = 36 // Minimum dialog width
	maxIterDialogMaxWidth     = 84 // Maximum dialog width
)

type maxIterationsDialog struct {
	BaseDialog

	maxIterations int
	sessionID     string
	requestID     string
	keyMap        ConfirmKeyMap
}

// NewMaxIterationsDialog creates a new max iterations confirmation dialog
func NewMaxIterationsDialog(maxIterations int, sessionID, requestID string) Dialog {
	return &maxIterationsDialog{
		maxIterations: maxIterations,
		sessionID:     sessionID,
		requestID:     requestID,
		keyMap:        DefaultConfirmKeyMap(),
	}
}

// Init initializes the max iterations confirmation dialog
func (d *maxIterationsDialog) Init() tea.Cmd {
	return nil
}

func (d *maxIterationsDialog) layout() DialogLayout {
	view := d.View()
	row, col := d.CenterDialog(view)
	return NewDialogLayout(view, row, col)
}

// OutsideClickDismissCmd keeps this runtime decision open until explicitly answered.
func (d *maxIterationsDialog) OutsideClickDismissCmd() tea.Cmd { return nil }

func (d *maxIterationsDialog) response(request runtime.ResumeRequest) messages.InteractionResponseMsg {
	return messages.InteractionResponseMsg{SessionID: d.sessionID, Response: runtime.InteractionResponse{InteractionID: d.requestID, Kind: runtime.InteractionMaxIterations, Resume: request}}
}

func (d *maxIterationsDialog) CancelDialogCmd() tea.Cmd { return d.rejectCmd() }

func (d *maxIterationsDialog) approveCmd() tea.Cmd {
	return core.CmdHandler(d.response(runtime.ResumeApprove()))
}

func (d *maxIterationsDialog) rejectCmd() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(d.response(runtime.ResumeReject(""))),
	)
}

// Update handles messages for the max iterations confirmation dialog
func (d *maxIterationsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
			dl := d.layout()
			if d.CloseButtonHit(msg, dl) {
				return d, d.rejectCmd()
			}
			if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
				return d.Update(action)
			}
		}

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, d.CancelDialogCmd()
		}
		switch d.HandleConfirmKey(msg, d.keyMap) {
		case ConfirmKeyConfirmed:
			if !d.claimResponse() {
				return d, nil
			}
			return d, ConfirmAndClose(d.approveCmd())
		case ConfirmKeyCancelled:
			return d, d.rejectCmd()
		case ConfirmKeyFocusToggled:
			return d, nil
		}
	}

	return d, nil
}

// Position returns the dialog position (centered)
func (d *maxIterationsDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the max iterations confirmation dialog
func (d *maxIterationsDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth := d.ComputeDialogWidth(maxIterDialogWidthPercent, maxIterDialogMinWidth, maxIterDialogMaxWidth)
	contentWidth := dialogWidth - styles.DialogWarningStyle.GetHorizontalFrameSize()
	bodyWidth := d.BodyContentWidth(dialogWidth)

	infoText := fmt.Sprintf("Max Iterations: %d", d.maxIterations)
	messageText := "The agent may be stuck in a loop. This can happen with smaller or less capable models."
	questionText := "Do you want to continue for 10 more iterations?"

	header = RenderTitle("Maximum Iterations Reached", contentWidth, styles.DialogTitleStyle)
	body = NewContent(bodyWidth).
		AddContent(styles.DialogContentStyle.Render(wrapDisplayText(infoText, bodyWidth))).
		AddSpace().AddContent(styles.DialogContentStyle.Render(wrapDisplayText(messageText, bodyWidth))).
		AddSpace().AddQuestion(questionText).Build()
	return styles.DialogWarningStyle, dialogWidth, header, body, d.RenderConfirmButtons(contentWidth)
}

func (d *maxIterationsDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *maxIterationsDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

// wrapDisplayText wraps text based on display cell width.
func wrapDisplayText(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return text
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var lines []string
	var current string
	for _, w := range words {
		if lipgloss.Width(current) == 0 {
			current = w
			continue
		}
		if lipgloss.Width(current+" "+w) <= maxWidth {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n")
}

// DialogClosable keeps this mandatory decision free of dismiss chrome.
func (d *maxIterationsDialog) DialogClosable() bool { return false }

func (d *maxIterationsDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}
