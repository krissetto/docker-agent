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
	SessionID      string
	RememberChoice bool
}

// RememberCloseTabNoMsg remembers an explicitly chosen No without closing a tab.
type RememberCloseTabNoMsg struct{}

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

	sessionID       string
	keyMap          closeRootWithSubagentsKeyMap
	rememberChoice  bool
	rememberFocused bool
	rememberLine    int
	rememberWidth   int
	rememberHeight  int
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
	d.prepareLayout()
	return nil
}

func (d *closeRootWithSubagentsDialog) CancelDialogCmd() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return d.BaseDialog.CancelDialogCmd()
}

func (d *closeRootWithSubagentsDialog) choose(yes bool) tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	if yes {
		return ConfirmAndClose(core.CmdHandler(CloseRootWithSubagentsConfirmedMsg{SessionID: d.sessionID, RememberChoice: d.rememberChoice}))
	}
	if d.rememberChoice {
		return ConfirmAndClose(core.CmdHandler(RememberCloseTabNoMsg{}))
	}
	return d.BaseDialog.CancelDialogCmd()
}

func (d *closeRootWithSubagentsDialog) focusRemember() {
	d.BlurActions()
	d.rememberFocused = true
	d.EnsureBodyLineVisible(d.rememberLine)
	d.MarkVisualDirty()
}

func (d *closeRootWithSubagentsDialog) rememberAt(x, y int) bool {
	left, top, width, height := d.BodyScrollBounds()
	line := y - top + d.BodyScrollOffset()
	return x >= left && x < left+min(width-2, d.rememberWidth) && y >= top && y < top+height &&
		line >= d.rememberLine && line < d.rememberLine+d.rememberHeight
}

// Update handles messages for the dialog.
func (d *closeRootWithSubagentsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
			if d.rememberAt(msg.X, msg.Y) {
				d.focusRemember()
				d.rememberChoice = !d.rememberChoice
				return d, nil
			}
			if action, ok := d.ActionKeyAt(msg.X, msg.Y, dl); ok {
				d.BlurActions()
				d.rememberFocused = false
				return d.Update(action)
			}
		}

	case tea.KeyPressMsg:
		if msg.Code == tea.KeyTab {
			backward := msg.Mod&tea.ModShift != 0
			switch {
			case d.rememberFocused:
				d.rememberFocused = false
				d.FocusActions(backward)
			case d.ActionsFocused():
				if (!backward && d.focusedAction == len(d.actions)-1) || (backward && d.focusedAction == 0) {
					d.focusRemember()
				} else {
					d.FocusActions(!backward)
				}
			case backward:
				d.FocusActions(true)
			default:
				d.focusRemember()
			}
			d.MarkVisualDirty()
			return d, nil
		}
		if d.rememberFocused {
			switch msg.Code {
			case tea.KeySpace, tea.KeyEnter:
				d.rememberChoice = !d.rememberChoice
				d.MarkVisualDirty()
				return d, nil
			case tea.KeyDown, tea.KeyRight:
				d.rememberFocused = false
				d.FocusActions(false)
				return d, nil
			case tea.KeyUp, tea.KeyLeft:
				d.rememberFocused = false
				d.FocusActions(true)
				return d, nil
			}
		}
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
			cmd := d.choose(true)
			return d, cmd
		case ConfirmKeyCancelled:
			if msg.Code == tea.KeyEscape {
				cmd := d.CancelDialogCmd()
				return d, cmd
			}
			cmd := d.choose(false)
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
	checkbox := "[ ] Remember this choice"
	if d.rememberChoice {
		checkbox = "[x] Remember this choice"
	}
	checkboxStyle := styles.BaseStyle.Foreground(styles.TextPrimary)
	if d.rememberFocused {
		checkboxStyle = checkboxStyle.Foreground(styles.SelectedFg).Background(styles.Selected)
	}
	d.rememberLine = lipgloss.Height(body) + 1
	d.rememberWidth = lipgloss.Width(checkbox)
	d.rememberHeight = lipgloss.Height(checkboxStyle.Width(bodyWidth).Render(checkbox))
	body += "\n\n" + checkboxStyle.Width(bodyWidth).Render(checkbox)
	body += "\n" + styles.MutedStyle.Width(bodyWidth).Render("Tab to focus · Space to toggle")
	footer = d.RenderActions(contentWidth,
		Action{Label: "No", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}, Default: !d.rememberFocused, HideShortcut: true},
		Action{Label: "Yes", Key: tea.KeyPressMsg{Code: 'y', Text: "y"}},
	)
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
