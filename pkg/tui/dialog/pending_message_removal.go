package dialog

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type pendingMessageRemovalDialog struct {
	BaseDialog
	confirm tea.Cmd
}

// NewPendingMessageRemovalDialog requires an explicit affirmative action before withdrawal.
func NewPendingMessageRemovalDialog(confirm tea.Cmd) Dialog {
	return &pendingMessageRemovalDialog{BaseDialog: BaseDialog{bodyCompactTitle: true}, confirm: confirm}
}

func (d *pendingMessageRemovalDialog) Init() tea.Cmd { return nil }

func (d *pendingMessageRemovalDialog) CancelDialogCmd() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return core.CmdHandler(CloseDialogByModelMsg{Model: d})
}

func (d *pendingMessageRemovalDialog) Cleanup() { d.responseSent = true }

func (d *pendingMessageRemovalDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if d.responseSent {
		return d, nil
	}
	switch msg.(type) {
	case tea.KeyPressMsg, tea.MouseClickMsg:
		defer d.prepareLayout()
	}
	if handled, cmd := d.UpdateBodyScroll(msg); handled {
		return d, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		view := d.View()
		row, col := d.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		if d.CloseButtonHit(msg, dl) {
			return d, d.CancelDialogCmd()
		}
		if action, hit := d.ActionKeyAt(msg.X, msg.Y, dl); hit {
			d.BlurActions()
			return d.Update(action)
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
		if HandleQuit(msg) != nil {
			return d, d.CancelDialogCmd()
		}
		switch d.HandleConfirmKey(msg, DefaultConfirmKeyMap()) {
		case ConfirmKeyConfirmed:
			if d.claimResponse() {
				return d, tea.Sequence(core.CmdHandler(CloseDialogByModelMsg{Model: d}), d.confirm)
			}
		case ConfirmKeyCancelled:
			return d, d.CancelDialogCmd()
		case ConfirmKeyNone, ConfirmKeyFocusToggled:
		}
	}
	return d, nil
}

func (d *pendingMessageRemovalDialog) content() (width int, header, body, footer string) {
	width = d.ComputeDialogWidth(50, 36, 60)
	contentWidth := d.ContentWidth(width, 2)
	header = RenderDialogHeader("Remove queued message", contentWidth, styles.DialogTitleStyle)
	body = styles.DialogQuestionStyle.Width(d.BodyContentWidth(width)).Render("Remove this queued message?")
	padding := min(2, max(0, (contentWidth-3)/2))
	footer = d.renderActions(contentWidth, lipgloss.Right, 2, padding,
		Action{Label: "Cancel", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}, Default: true, HideShortcut: true},
		Action{Label: "Remove", Key: tea.KeyPressMsg{Code: 'y', Text: "y"}, HideShortcut: true})
	return width, header, body, footer
}

func (d *pendingMessageRemovalDialog) View() string {
	width, header, body, footer := d.content()
	return d.RenderScrollableBody(styles.DialogStyle.Padding(1, 2), width, header, body, footer)
}

func (d *pendingMessageRemovalDialog) prepareLayout() {
	width, header, body, footer := d.content()
	d.PrepareScrollableBody(styles.DialogStyle.Padding(1, 2), width, header, body, footer)
}

func (d *pendingMessageRemovalDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}

func (d *pendingMessageRemovalDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }
