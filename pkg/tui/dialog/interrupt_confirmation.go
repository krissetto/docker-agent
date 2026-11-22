package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// InterruptConfirmedMsg is sent when the user confirms they want to
// interrupt the current stream.
type InterruptConfirmedMsg struct{}

type interruptConfirmationKeyMap struct {
	Yes key.Binding
	No  key.Binding
	Esc key.Binding
}

func defaultInterruptConfirmationKeyMap() interruptConfirmationKeyMap {
	return interruptConfirmationKeyMap{
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

type interruptConfirmationDialog struct {
	BaseDialog

	keyMap interruptConfirmationKeyMap
}

// NewInterruptConfirmationDialog creates a new interrupt confirmation dialog.
func NewInterruptConfirmationDialog() Dialog {
	return &interruptConfirmationDialog{
		keyMap: defaultInterruptConfirmationKeyMap(),
	}
}

// Init initializes the interrupt confirmation dialog.
func (d *interruptConfirmationDialog) Init() tea.Cmd {
	return nil
}

// Update handles messages for the interrupt confirmation dialog.
func (d *interruptConfirmationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			view := d.View()
			row, col := d.CenterDialog(view)
			if action, ok := d.ApplyActionClick(msg, view, row, col, styles.DialogWarningStyle); ok {
				return d.Update(action)
			}
		}
	case tea.KeyPressMsg:
		action, handled := d.ApplyActionKey(msg, d.View)
		if handled {
			return d, nil
		}
		msg = action
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(InterruptConfirmedMsg{}),
			)
		case key.Matches(msg, d.keyMap.No), key.Matches(msg, d.keyMap.Esc):
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}

	return d, nil
}

// Position returns the dialog position (centered).
func (d *interruptConfirmationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the interrupt confirmation dialog.
func (d *interruptConfirmationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(50, 30, 50)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Interrupt").
		AddSeparator().
		AddSpace().
		AddQuestion("Stop the current response?").
		AddSpace().
		AddContent(d.Actions.Render(contentWidth, lipgloss.Right, []Action{{Label: "No", Key: 'n'}, {Label: "Yes", Key: 'y'}}...)).
		Build()

	return styles.DialogStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(content)
}
