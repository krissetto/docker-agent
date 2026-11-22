package dialog

import (
	"context"
	"log/slog"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// URLElicitationDialog handles URL-based MCP elicitation requests.
// It displays a URL for the user to visit and waits for confirmation.
type URLElicitationDialog struct {
	BaseDialog

	ctx func() context.Context

	message       string
	url           string
	elicitationID string
	keyMap        ConfirmKeyMap
	escape        key.Binding
	openBrowser   key.Binding
}

// NewURLElicitationDialog creates a new URL elicitation dialog. elicitationID
// is variadic for the same backward-compatibility reason as
// NewElicitationDialog (see firstElicitationID); at most the first value is
// meaningful.
func NewURLElicitationDialog(ctx context.Context, message, url string, elicitationID ...string) Dialog {
	return &URLElicitationDialog{
		ctx:           func() context.Context { return context.WithoutCancel(ctx) },
		message:       message,
		url:           url,
		elicitationID: firstElicitationID(elicitationID),
		keyMap:        DefaultConfirmKeyMap(),
		escape:        key.NewBinding(key.WithKeys("esc")),
		openBrowser: key.NewBinding(
			key.WithKeys("o"),
			key.WithHelp("o", "open"),
		),
	}
}

func (d *URLElicitationDialog) Init() tea.Cmd {
	return nil
}

func (d *URLElicitationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			view := d.View()
			row, col := d.CenterDialog(view)
			if action, ok := d.ApplyActionClick(msg, view, row, col, styles.DialogStyle); ok {
				return d.Update(action)
			}
			if d.url != "" {
				cmd := d.openURLInBrowser()
				return d, cmd
			}
		}
	case tea.KeyPressMsg:
		action, handled := d.ApplyActionKey(msg, d.View)
		if handled {
			return d, nil
		}
		msg = action
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			cmd := d.respond(tools.ElicitationActionAccept)
			return d, cmd
		case key.Matches(msg, d.keyMap.No):
			cmd := d.respond(tools.ElicitationActionDecline)
			return d, cmd
		case key.Matches(msg, d.escape):
			cmd := d.CancelDialogCmd()
			return d, cmd
		case key.Matches(msg, d.openBrowser):
			cmd := d.openURLInBrowser()
			return d, cmd
		}
	}
	return d, nil
}

func (d *URLElicitationDialog) respond(action tools.ElicitationAction) tea.Cmd {
	return CloseWithElicitationResponse(action, nil, d.elicitationID)
}

func (d *URLElicitationDialog) openURLInBrowser() tea.Cmd {
	return func() tea.Msg {
		if d.url == "" {
			return nil
		}
		if err := browser.Open(d.ctx(), d.url); err != nil {
			slog.Error("Failed to open URL in browser", "url", d.url, "error", err)
		}
		return nil
	}
}

func (d *URLElicitationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *URLElicitationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(70, 50, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth)
	content.AddTitle("MCP Server Request")
	content.AddSeparator()

	// Message from server
	content.AddContent(styles.DialogContentStyle.Width(contentWidth).Render(d.message))
	content.AddSpace()

	// URL to visit
	if d.url != "" {
		content.AddContent(styles.DialogContentStyle.Foreground(styles.TextMuted).Render("Please visit:"))
		content.AddContent(styles.InfoStyle.Width(contentWidth).Render(d.url))
		content.AddSpace()
	}

	content.AddHelp("Press Y when you have completed the action, or N to decline.")
	content.AddSpace()
	content.AddContent(d.Actions.Render(contentWidth, lipgloss.Right, []Action{{Label: "Cancel", Key: tea.KeyEscape}, {Label: "Decline", Key: 'n', Badge: "N"}, {Label: "Open", Key: 'o', Badge: "O"}, {Label: "Confirm", Key: 'y', Badge: "Y"}}...))

	return styles.DialogStyle.Width(dialogWidth).Render(content.Build())
}

func (d *URLElicitationDialog) CancelDialogCmd() tea.Cmd {
	return d.respond(tools.ElicitationActionCancel)
}
