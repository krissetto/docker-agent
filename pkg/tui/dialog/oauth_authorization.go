package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

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

func (d *oauthAuthorizationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		model, cmd, handled := HandleConfirmKeys(msg, d.keyMap,
			func() (layout.Model, tea.Cmd) { return d.respond(tools.ElicitationActionAccept) },
			func() (layout.Model, tea.Cmd) { return d.respond(tools.ElicitationActionDecline) },
		)
		if handled {
			return model, cmd
		}
	}
	return d, nil
}

func (d *oauthAuthorizationDialog) respond(action tools.ElicitationAction) (layout.Model, tea.Cmd) {
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

func (d *oauthAuthorizationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	serverInfo := styles.InfoStyle.Width(contentWidth).Render(fmt.Sprintf("Server: %s (remote)", d.serverURL))
	description := styles.DialogContentStyle.Width(contentWidth).Render("This server requires OAuth authentication to access its tools. Your browser will open automatically to complete the authorization process.")
	content := NewContent(contentWidth).
		AddContent(styles.DialogTitleInfoStyle.Width(contentWidth).Render("🔐 OAuth Authorization Required")).
		AddSpace().AddContent(serverInfo).AddSpace().AddContent(description).AddSpace().
		AddHelp("After authorizing in your browser, return here and the agent will continue automatically.").
		AddSpace().AddHelpKeys("Y", "authorize", "N", "decline").Build()
	return styles.DialogWarningStyle.Padding(1, 2).Width(dialogWidth).Render(content)
}
