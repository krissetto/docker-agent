package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ExitConfirmedMsg is sent when the user confirms they want to exit.
type ExitConfirmedMsg struct{}

type exitConfirmationKeyMap struct {
	Yes key.Binding
	No  key.Binding
	Esc key.Binding
}

func defaultExitConfirmationKeyMap() exitConfirmationKeyMap {
	// Pressing the quit key again confirms exit, so fold the configured quit
	// keys into the Yes binding.
	yesKeys := append([]string{"y", "Y"}, core.GetKeys().Quit.Keys()...)

	return exitConfirmationKeyMap{
		Yes: key.NewBinding(
			key.WithKeys(yesKeys...),
			key.WithHelp("Y", "yes"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "no"),
		),
		Esc: key.NewBinding(
			key.WithKeys("esc"),
		),
	}
}

type exitConfirmationDialog struct {
	BaseDialog

	keyMap exitConfirmationKeyMap
}

// NewExitConfirmationDialog creates a new exit confirmation dialog.
func NewExitConfirmationDialog() Dialog {
	return &exitConfirmationDialog{
		keyMap: defaultExitConfirmationKeyMap(),
	}
}

// Init initializes the exit confirmation dialog.
func (d *exitConfirmationDialog) Init() tea.Cmd {
	return nil
}

func (d *exitConfirmationDialog) layout() DialogLayout {
	view := d.View()
	row, col := d.CenterDialog(view)
	return NewDialogLayout(view, row, col)
}

// confirmExitCmd performs the affirmative exit transaction. Only the Yes
// button/key paths may call it.
func confirmExitCmd() tea.Cmd {
	return ConfirmAndClose(core.CmdHandler(ExitConfirmedMsg{}))
}

// OutsideClickDismissCmd cancels the confirmation like Escape/X. An outside
// click is dismissal only and can never confirm exit.
func (d *exitConfirmationDialog) OutsideClickDismissCmd() tea.Cmd { return d.CancelDialogCmd() }

// Update handles messages for the exit confirmation dialog.
func (d *exitConfirmationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			dl := d.layout()
			if d.CloseButtonHit(msg, dl) {
				return d, func() tea.Msg { return CloseDialogMsg{} }
			}
			if cmd := d.HandleConfirmButtonsClick(msg, dl, styles.DialogStyle.Padding(1, 2), core.CmdHandler(ExitConfirmedMsg{})); cmd != nil {
				return d, cmd
			}
		}

	case tea.MouseMotionMsg:
		d.HandleMouseMotion(msg.X, msg.Y, d.layout())
		return d, nil

	case tea.KeyPressMsg:
		switch d.HandleConfirmKey(msg, ConfirmKeyMap{Yes: d.keyMap.Yes, No: d.keyMap.No}) {
		case ConfirmKeyConfirmed:
			return d, confirmExitCmd()
		case ConfirmKeyCancelled:
			return d, func() tea.Msg { return CloseDialogMsg{} }
		case ConfirmKeyFocusToggled:
			return d, nil
		}
	}

	return d, nil
}

// Position returns the dialog position (centered).
func (d *exitConfirmationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the exit confirmation dialog.
func (d *exitConfirmationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(50, 30, 50)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Exit").
		AddSeparator().
		AddSpace().
		AddQuestion("Do you want to exit?").
		AddSpace().
		AddContent(d.RenderConfirmButtons(contentWidth)).
		Build()

	return d.RenderCard(styles.DialogStyle.Padding(1, 2), dialogWidth, content)
}
