//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
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

	message     string
	url         string
	ref         ElicitationRef
	keyMap      ConfirmKeyMap
	escape      key.Binding
	openBrowser key.Binding
}

// NewURLElicitationDialog creates a new URL elicitation dialog answering ref.
func NewURLElicitationDialog(ctx context.Context, message, url string, ref ElicitationRef) Dialog {
	return &URLElicitationDialog{
		ctx:     func() context.Context { return context.WithoutCancel(ctx) },
		message: message,
		url:     url,
		ref:     ref,
		keyMap:  DefaultConfirmKeyMap(),
		escape:  key.NewBinding(key.WithKeys("esc")),
		openBrowser: key.NewBinding(
			key.WithKeys("o"),
			key.WithHelp("o", "open"),
		),
	}
}

func (d *URLElicitationDialog) Init() tea.Cmd {
	return nil
}

func (d *URLElicitationDialog) CancelDialogCmd() tea.Cmd {
	return d.respond(tools.ElicitationActionCancel)
}

// OutsideClickDismissCmd cancels exactly like Escape, atomically closing the
// dialog and answering the runtime waiter.
func (d *URLElicitationDialog) OutsideClickDismissCmd() tea.Cmd {
	return d.respond(tools.ElicitationActionCancel)
}

func (d *URLElicitationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
	case tea.KeyPressMsg:
		if action, handled := d.HandleActionKey(msg); handled {
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
		if cmd := HandleQuit(msg); cmd != nil {
			return d, d.CancelDialogCmd()
		}
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			cmd := d.respond(tools.ElicitationActionAccept)
			return d, cmd
		case key.Matches(msg, d.keyMap.No):
			cmd := d.respond(tools.ElicitationActionDecline)
			return d, cmd
		case key.Matches(msg, d.escape):
			cmd := d.respond(tools.ElicitationActionCancel)
			return d, cmd
		case key.Matches(msg, d.openBrowser):
			cmd := d.openURLInBrowser()
			return d, cmd
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		view := d.View()
		row, col := d.CenterDialog(view)
		if d.CloseButtonHit(msg, NewDialogLayout(view, row, col)) {
			return d, d.respond(tools.ElicitationActionCancel)
		}
		if action, ok := d.ActionKeyAt(msg.X, msg.Y, NewDialogLayout(view, row, col)); ok {
			d.BlurActions()
			return d.Update(action)
		}
	}
	return d, nil
}

func (d *URLElicitationDialog) respond(action tools.ElicitationAction) tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return CloseWithElicitationResponse(action, nil, d.ref)
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

func (d *URLElicitationDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth := d.ComputeDialogWidth(70, 50, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	bodyWidth := d.BodyContentWidth(dialogWidth)

	content := NewContent(bodyWidth)
	header = RenderTitle("MCP Server Request", contentWidth, styles.DialogTitleStyle)

	// Message from server
	if d.message != "" {
		content.AddContent(styles.DialogContentStyle.Width(bodyWidth).Render(d.message))
		content.AddSpace()
	}

	// URL to visit
	if d.url != "" {
		content.AddContent(styles.DialogContentStyle.Foreground(styles.TextMuted).Render("Please visit:"))
		content.AddContent(styles.InfoStyle.Width(bodyWidth).Render(d.url))
		content.AddSpace()
	}

	content.AddContent(styles.DialogContentStyle.Width(bodyWidth).Render("Confirm when you have completed the action."))
	actions := []Action{{Label: "Decline", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}}}
	if d.url != "" {
		actions = append(actions, Action{Label: "Open", Key: tea.KeyPressMsg{Code: 'o', Text: "o"}})
	}
	actions = append(actions, Action{Label: "Confirm", Key: tea.KeyPressMsg{Code: 'y', Text: "y"}})
	footer = d.RenderActions(contentWidth, actions...)
	return styles.DialogStyle, dialogWidth, header, content.Build(), footer
}

func (d *URLElicitationDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *URLElicitationDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

func (d *URLElicitationDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}
