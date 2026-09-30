//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type oauthAuthorizationDialog struct {
	BaseDialog

	serverURL string
	ref       ElicitationRef
	keyMap    ConfirmKeyMap
}

func NewOAuthAuthorizationDialog(serverURL string, ref ElicitationRef) Dialog {
	return &oauthAuthorizationDialog{serverURL: serverURL, ref: ref, keyMap: DefaultConfirmKeyMap()}
}

func (d *oauthAuthorizationDialog) Init() tea.Cmd { return nil }

// CancelDialogCmd declines exactly like the No key, atomically closing
// the dialog and answering the runtime waiter.
func (d *oauthAuthorizationDialog) CancelDialogCmd() tea.Cmd {
	_, cmd := d.respond(tools.ElicitationActionDecline)
	return cmd
}

func (d *oauthAuthorizationDialog) OutsideClickDismissCmd() tea.Cmd {
	return d.CancelDialogCmd()
}

func (d *oauthAuthorizationDialog) layout() DialogLayout {
	view := d.View()
	row, col := d.CenterDialog(view)
	return NewDialogLayout(view, row, col)
}

// Update handles messages for the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
				return d.respond(tools.ElicitationActionDecline)
			}
			if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
				d.BlurActions()
				return d.Update(action)
			}
		}

	case tea.KeyPressMsg:
		if !d.ActionsFocused() {
			switch msg.Code {
			case tea.KeyLeft, tea.KeyRight, tea.KeyUp, tea.KeyDown:
				d.FocusDefaultAction()
			}
		}
		if action, handled := d.HandleActionKey(msg); handled {
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
		if cmd := HandleQuit(msg); cmd != nil {
			return d, d.CancelDialogCmd()
		}
		switch d.HandleConfirmKey(msg, d.keyMap) {
		case ConfirmKeyConfirmed:
			return d.respond(tools.ElicitationActionAccept)
		case ConfirmKeyCancelled:
			return d.respond(tools.ElicitationActionDecline)
		case ConfirmKeyFocusToggled:
			return d, nil
		}
	}
	return d, nil
}

func (d *oauthAuthorizationDialog) respond(action tools.ElicitationAction) (layout.Model, tea.Cmd) {
	if !d.claimResponse() {
		return d, nil
	}
	return d, tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.InteractionResponseMsg{
			SessionID: d.ref.SessionID,
			Response: runtime.InteractionResponse{
				InteractionID: d.ref.RequestID,
				Kind:          runtime.InteractionElicitation, ElicitationID: d.ref.ElicitationID,
				Elicitation: runtime.ElicitationResult{Action: action},
			},
		}),
	)
}

func (d *oauthAuthorizationDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *oauthAuthorizationDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth := d.ComputeDialogWidth(60, 40, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	bodyWidth := d.BodyContentWidth(dialogWidth)
	serverInfo := styles.InfoStyle.Width(bodyWidth).Render(fmt.Sprintf("Server: %s (remote)", d.serverURL))
	description := styles.DialogContentStyle.Width(bodyWidth).Render("This server requires OAuth authentication to access its tools. Your browser will open automatically to complete the authorization process.")
	header = RenderDialogHeader("OAuth Authorization Required", contentWidth, styles.DialogTitleInfoStyle)
	body = NewContent(bodyWidth).AddContent(serverInfo).AddSpace().AddContent(description).
		AddSpace().AddContent(styles.DialogContentStyle.Width(bodyWidth).Render("After authorizing in your browser, return here and the agent will continue automatically.")).Build()
	footer = d.RenderActions(contentWidth,
		Action{Label: "Decline", Default: true, Key: tea.KeyPressMsg{Code: 'n', Text: "n"}},
		Action{Label: "Authorize", Key: tea.KeyPressMsg{Code: 'y', Text: "y"}},
	)
	return styles.DialogWarningStyle, dialogWidth, header, body, footer
}

func (d *oauthAuthorizationDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *oauthAuthorizationDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

func (d *oauthAuthorizationDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}
