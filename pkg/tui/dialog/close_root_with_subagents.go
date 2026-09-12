package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(CloseRootWithSubagentsConfirmedMsg{SessionID: d.sessionID}),
			)
		case key.Matches(msg, d.keyMap.No), key.Matches(msg, d.keyMap.Esc):
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}

	return d, nil
}

// Position returns the dialog position.
func (d *closeRootWithSubagentsDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the confirmation dialog.
func (d *closeRootWithSubagentsDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 50, 70)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Close session").
		AddSeparator().
		AddSpace().
		AddQuestion("This session has running subagents. Closing it will interrupt their current work and close their tabs. Continue?").
		AddSpace().
		AddHelpKeys("Y", "yes", "N", "no", "Esc", "cancel").
		Build()

	return styles.DialogStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(content)
}
