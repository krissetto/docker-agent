package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// CloseRootWithSubagentsConfirmedMsg is sent when the user confirms closing a
// root tab that owns running subagents.
type CloseRootWithSubagentsConfirmedMsg struct {
	SessionID string
}

type closeRootWithSubagentsKeyMap struct {
	Yes key.Binding
	No  key.Binding
	Esc key.Binding
}

func defaultCloseRootWithSubagentsKeyMap() closeRootWithSubagentsKeyMap {
	return closeRootWithSubagentsKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "yes"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "no"),
		),
		Esc: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("Esc", "cancel"),
		),
	}
}

type closeRootWithSubagentsDialog struct {
	BaseDialog

	sessionID string
	keyMap    closeRootWithSubagentsKeyMap
}

// NewCloseRootWithSubagentsDialog creates a confirmation dialog for closing a
// root session that owns running subagents.
func NewCloseRootWithSubagentsDialog(sessionID string) Dialog {
	return &closeRootWithSubagentsDialog{
		sessionID: sessionID,
		keyMap:    defaultCloseRootWithSubagentsKeyMap(),
	}
}

// Init initializes the dialog.
func (d *closeRootWithSubagentsDialog) Init() tea.Cmd {
	return nil
}

// Update handles messages for the dialog.
func (d *closeRootWithSubagentsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
			view := d.View()
			row, col := d.CenterDialog(view)
			dl := NewDialogLayout(view, row, col)
			if d.CloseButtonHit(msg, dl) {
				cmd := d.CancelDialogCmd()
				return d, cmd
			}
			if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
				return d.Update(action)
			}
		}

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}
		switch d.HandleConfirmKey(msg, ConfirmKeyMap{Yes: d.keyMap.Yes, No: d.keyMap.No}) {
		case ConfirmKeyConfirmed:
			return d, ConfirmAndClose(core.CmdHandler(CloseRootWithSubagentsConfirmedMsg{SessionID: d.sessionID}))
		case ConfirmKeyCancelled:
			cmd := d.CancelDialogCmd()
			return d, cmd
		case ConfirmKeyFocusToggled:
			return d, nil
		}
	}

	return d, nil
}

// Position returns the dialog position.
func (d *closeRootWithSubagentsDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the confirmation dialog.
func (d *closeRootWithSubagentsDialog) content() (style lipgloss.Style, width int, header, body, footer string) {
	dialogWidth := d.ComputeDialogWidth(60, 50, 70)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	bodyWidth := d.BodyContentWidth(dialogWidth)

	header = RenderTitle("Close session", contentWidth, styles.DialogTitleStyle)
	body = styles.DialogQuestionStyle.Width(bodyWidth).Render("This session has running subagents. Closing it will interrupt their current work and close their tabs. Continue?")
	footer = d.RenderConfirmButtons(contentWidth)
	return styles.DialogStyle.Padding(1, 2), dialogWidth, header, body, footer
}

func (d *closeRootWithSubagentsDialog) View() string {
	style, width, header, body, footer := d.content()
	return d.RenderScrollableBody(style, width, header, body, footer)
}

func (d *closeRootWithSubagentsDialog) prepareLayout() {
	style, width, header, body, footer := d.content()
	d.PrepareScrollableBody(style, width, header, body, footer)
}

func (d *closeRootWithSubagentsDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}
