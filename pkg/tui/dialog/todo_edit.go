package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type todoEditDialog struct {
	BaseDialog

	scope            messages.TodoScope
	id               string
	expected, latest string
	editorID         uint64
	requestID        uint64
	save             func(uint64, string, string) tea.Cmd
	input            *editor.Input
	saving           bool
	closed           bool
	saveError        string
	inputRow         int
}

// NewTodoEditDialog preserves a dirty draft until its own save result arrives.
func NewTodoEditDialog(scope messages.TodoScope, id, content string, editorID uint64, save func(uint64, string, string) tea.Cmd) Dialog {
	input := editor.NewInput(editor.InputConfig{Unlimited: true, NewlineKeys: []string{"ctrl+j", "shift+enter"}})
	input.SetValue(content)
	input.Focus()
	return &todoEditDialog{scope: scope, id: id, expected: content, latest: content, editorID: editorID, save: save, input: input}
}

func (d *todoEditDialog) Init() tea.Cmd { return d.input.Init() }

// TodoEditID identifies this editor across asynchronous save results.
func (d *todoEditDialog) TodoEditID() uint64 { return d.editorID }

func (d *todoEditDialog) CancelDialogCmd() tea.Cmd {
	if d.closed {
		return nil
	}
	// Closing cannot roll back an already accepted backend save; it only prevents late UI effects.
	d.closed = true
	return core.CmdHandler(CloseDialogByModelMsg{Model: d})
}

func (d *todoEditDialog) Cleanup() { d.closed = true; d.input.Blur() }

func (d *todoEditDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}

func (d *todoEditDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if d.closed {
		return d, nil
	}
	switch msg := msg.(type) {
	case messages.TodosSnapshotMsg:
		if msg.Scope == d.scope && msg.Err == nil {
			for _, item := range msg.Todos {
				if item.ID == d.id {
					d.latest = item.Description
					break
				}
			}
		}
		return d, nil
	case messages.TodoSaveResultMsg:
		if msg.EditorID != d.editorID || msg.RequestID != d.requestID || msg.Scope != d.scope || msg.ID != d.id || !d.saving {
			return d, nil
		}
		d.saving = false
		for _, item := range msg.Todos {
			if item.ID == d.id {
				d.latest = item.Description
			}
		}
		if msg.Err != nil {
			d.saveError = msg.Err.Error()
			d.BlurActions()
			d.input.Focus()
			d.prepareLayout()
			return d, nil
		}
		cmd := d.CancelDialogCmd()
		return d, cmd
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		view := d.View()
		row, col := d.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		if d.CloseButtonHit(msg, dl) {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}
		if action, hit := d.ActionKeyAt(msg.X, msg.Y, dl); hit {
			d.BlurActions()
			return d.Update(action)
		}
		x, y, width, height := d.BodyScrollBounds()
		inputY := y + d.inputRow - d.BodyScrollOffset()
		if !d.saving && msg.X >= x && msg.X < x+max(1, width-2) && msg.Y >= max(y, inputY) && msg.Y < min(y+height, inputY+d.input.SurfaceHeight()) {
			d.BlurActions()
			d.input.Focus()
			d.input.PlaceSurfaceCursor(msg.X-x, msg.Y-inputY)
			d.prepareLayout()
		}
		return d, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+r" && !d.saving {
			d.expected = d.latest
			d.saveError = "Reloaded current description for retry; your draft is unchanged."
			d.prepareLayout()
			return d, nil
		}
		if msg.Code == tea.KeyEscape {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}
		if d.ActionsFocused() || msg.Code == tea.KeyTab {
			if action, handled := d.HandleActionKey(msg); handled {
				if d.ActionsFocused() {
					d.input.Blur()
				} else if !d.saving {
					d.input.Focus()
				}
				if action.Code == 0 {
					d.prepareLayout()
					return d, nil
				}
				msg = action
			}
		}
		if msg.Code == tea.KeyEscape {
			return d, d.CancelDialogCmd()
		}
		if msg.Code == tea.KeyEnter && (msg.Mod == 0 || msg.Mod == tea.ModCtrl) {
			cmd := d.beginSave()
			return d, cmd
		}
		if d.saving || d.ActionsFocused() {
			return d, nil
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		d.prepareLayout()
		return d, cmd
	case tea.PasteMsg:
		if d.saving || d.ActionsFocused() {
			return d, nil
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		d.prepareLayout()
		return d, cmd
	case tea.MouseWheelMsg:
		delta := 0
		switch msg.Button {
		case tea.MouseWheelUp:
			delta = -3
		case tea.MouseWheelDown:
			delta = 3
		}
		d.scrollInput(msg.X, msg.Y, delta)
		return d, nil
	case messages.WheelCoalescedMsg:
		d.scrollInput(msg.X, msg.Y, msg.Delta)
		return d, nil
	}
	if handled, cmd := d.UpdateBodyScroll(msg); handled {
		return d, cmd
	}
	if !d.saving && !d.ActionsFocused() {
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		d.prepareLayout()
		return d, cmd
	}
	return d, nil
}

func (d *todoEditDialog) beginSave() tea.Cmd {
	if d.saving || d.closed {
		return nil
	}
	if d.save == nil {
		d.saveError = "Editing this todo is not supported."
		d.prepareLayout()
		return nil
	}
	if strings.TrimSpace(d.input.Value()) == "" {
		d.saveError = "Description cannot be blank."
		d.prepareLayout()
		return nil
	}
	d.saving = true
	d.saveError = ""
	d.input.Blur()
	d.requestID++
	cmd := d.save(d.requestID, d.expected, d.input.Value())
	if cmd == nil {
		d.saving = false
		d.saveError = "The todo could not be saved. Try again."
		d.input.Focus()
	}
	d.prepareLayout()
	return cmd
}

func (d *todoEditDialog) content() (width int, header, body, footer string) {
	width = d.ComputeDialogWidth(85, 50, 110)
	header = RenderDialogHeader("Edit todo", d.ContentWidth(width, 2), styles.DialogTitleStyle)
	body = d.input.SurfaceView()
	if d.saveError != "" {
		body = styles.DialogContentStyle.Foreground(styles.Error).Width(d.BodyContentWidth(width)).Render(d.saveError+"\nCtrl+R reloads the current description for retry; draft retained.") + "\n" + body
	}
	label := "Save"
	if d.saving {
		label = "Saving…"
	}
	contentWidth := d.ContentWidth(width, 2)
	padding := min(2, max(0, (contentWidth-3)/2))
	footer = d.renderActions(contentWidth, lipgloss.Right, 2, padding,
		Action{Label: "Cancel", HideShortcut: true, HideFocusHint: true, Key: tea.KeyPressMsg{Code: tea.KeyEscape}},
		Action{Label: label, Default: true, HideShortcut: true, HideFocusHint: true, Key: tea.KeyPressMsg{Code: tea.KeyEnter}, Disabled: d.saving})
	return width, header, body, footer
}

func (d *todoEditDialog) prepareLayout() {
	width := d.ComputeDialogWidth(85, 50, 110)
	d.inputRow = 0
	if d.saveError != "" {
		d.inputRow = lipgloss.Height(styles.DialogContentStyle.Width(d.BodyContentWidth(width)).Render(d.saveError + "\nCtrl+R reloads the current description for retry; draft retained."))
	}
	// Measure at the final width without temporarily shrinking the cursor viewport.
	d.input.SetSize(d.BodyContentWidth(width), max(d.input.SurfaceHeight(), 3+styles.EditorStyle.GetVerticalFrameSize()))
	height := max(3, d.input.ContentLineCount()) + d.input.Frame().GetVerticalFrameSize()
	width, header, _, footer := d.content()
	// Allocate rows before resizing the editor, so a temporary oversized viewport
	// cannot shift its scroll offset on every keystroke or wheel event.
	body := strings.Repeat("\n", max(0, min(height+d.inputRow, d.Height())-1))
	d.PrepareScrollableBody(styles.DialogStyle, width, header, body, footer)
	d.input.SetSize(d.BodyContentWidth(width), max(1, d.bodyScroll.VisibleHeight()-d.inputRow))
	width, header, body, footer = d.content()
	d.PrepareScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *todoEditDialog) View() string {
	width, header, body, footer := d.content()
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *todoEditDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *todoEditDialog) placeCursor(cell, row int) {
	d.input.PlaceCursor(cell, row)
}

func (d *todoEditDialog) scrollInput(x, y, delta int) {
	left, top, width, height := d.BodyScrollBounds()
	if d.saving || x < left || x >= left+width || y < top || y >= top+height || delta == 0 {
		return
	}
	d.input.ScrollByWheel(delta)
	d.prepareLayout()
}
