package dialog

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// CloseRootWithSubagentsConfirmedMsg closes the requested tab without stopping
// its subagents or closing their open tabs.
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

// NewCloseRootWithSubagentsDialog confirms closing a tab while its subagents
// and their open tabs remain available.
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

	header = RenderDialogHeader("Close tab", contentWidth, styles.DialogTitleStyle)
	body = styles.DialogQuestionStyle.Width(bodyWidth).Render("Close this tab? Subagents keep running and their open tabs stay open.")
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
