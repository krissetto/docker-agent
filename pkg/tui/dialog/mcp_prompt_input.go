package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// MCPPromptInputDialog implements Dialog for collecting MCP prompt parameters
type MCPPromptInputDialog struct {
	BaseDialog

	promptName       string
	promptInfo       mcptools.PromptInfo
	inputs           []textinput.Model
	arguments        []mcptools.PromptArgument
	currentInput     int
	keyMap           mcpPromptInputKeyMap
	fieldStarts      []int
	fieldHeights     []int
	bodyRow, bodyCol int
	reanchorFocus    bool
}

// mcpPromptInputKeyMap defines key bindings for the MCP prompt input dialog
type mcpPromptInputKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Escape key.Binding
	Tab    key.Binding
}

// defaultMCPPromptInputKeyMap returns default key bindings
func defaultMCPPromptInputKeyMap() mcpPromptInputKeyMap {
	return mcpPromptInputKeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "shift+tab"),
			key.WithHelp("↑/shift+tab", "previous field"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "tab"),
			key.WithHelp("↓/tab", "next field"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "execute"),
		),
		Escape: key.NewBinding(
			key.WithKeys("esc"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next field"),
		),
	}
}

// NewMCPPromptInputDialog creates a new MCP prompt input dialog
func NewMCPPromptInputDialog(promptName string, promptInfo mcptools.PromptInfo) Dialog {
	// Create text inputs for all arguments (both required and optional)
	var inputs []textinput.Model
	var arguments []mcptools.PromptArgument

	for _, arg := range promptInfo.Arguments {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.Placeholder = arg.Description
		ti.CharLimit = 500
		ti.SetWidth(50)

		inputs = append(inputs, ti)
		arguments = append(arguments, arg)
	}

	// Focus the first input if any
	if len(inputs) > 0 {
		inputs[0].Focus()
	}

	return &MCPPromptInputDialog{
		promptName:   promptName,
		promptInfo:   promptInfo,
		inputs:       inputs,
		arguments:    arguments,
		currentInput: 0,
		keyMap:       defaultMCPPromptInputKeyMap(),
	}
}

// Init initializes the MCP prompt input dialog
func (d *MCPPromptInputDialog) Init() tea.Cmd {
	return textinput.Blink
}

// Update handles messages for the MCP prompt input dialog
func (d *MCPPromptInputDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseClickMsg:
		defer d.prepareLayout()
	}

	var cmds []tea.Cmd
	if keyMsg, isKey := msg.(tea.KeyPressMsg); !isKey || keyMsg.String() == "pgup" || keyMsg.String() == "pgdown" {
		if handled, cmd := d.UpdateBodyScroll(msg); handled {
			return d, cmd
		}
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		if d.ActionsFocused() {
			return d, nil
		}
		// Forward paste to current text input
		if d.currentInput < len(d.inputs) {
			var cmd tea.Cmd
			d.inputs[d.currentInput], cmd = d.inputs[d.currentInput].Update(msg)
			d.ensureInputVisible()
			cmds = append(cmds, cmd)
		}
		return d, tea.Batch(cmds...)

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg)
		}
		return d, nil

	case tea.KeyPressMsg:
		backward := msg.String() == "shift+tab"
		boundary := (msg.String() == "tab" && d.currentInput >= len(d.inputs)-1) || (backward && d.currentInput == 0)
		if d.ActionsFocused() || boundary {
			wasFocused := d.ActionsFocused()
			if action, handled := d.HandleActionKey(msg); handled {
				if d.ActionsFocused() {
					for i := range d.inputs {
						d.inputs[i].Blur()
					}
				} else if wasFocused && len(d.inputs) > 0 {
					if backward {
						d.currentInput = len(d.inputs) - 1
					} else {
						d.currentInput = 0
					}
					d.inputs[d.currentInput].Focus()
					d.ensureInputVisible()
				}
				if action.Code == 0 {
					return d, nil
				}
				msg = action
			}
		}
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

		case key.Matches(msg, d.keyMap.Up):
			if d.currentInput > 0 {
				d.inputs[d.currentInput].Blur()
				d.currentInput--
				d.inputs[d.currentInput].Focus()
				d.ensureInputVisible()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Down), key.Matches(msg, d.keyMap.Tab):
			if d.currentInput < len(d.inputs)-1 {
				d.inputs[d.currentInput].Blur()
				d.currentInput++
				d.inputs[d.currentInput].Focus()
				d.ensureInputVisible()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			// Collect all input values
			arguments := make(map[string]string)
			for i, input := range d.inputs {
				arguments[d.arguments[i].Name] = strings.TrimSpace(input.Value())
			}

			if d.canExecute() {
				if !d.claimResponse() {
					return d, nil
				}
				cmds = append(cmds,
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(messages.MCPPromptMsg{
						PromptName: d.promptName,
						Arguments:  arguments,
					}),
				)
				return d, tea.Sequence(cmds...)
			}
			return d, nil

		default:
			// Update the current input
			if !d.ActionsFocused() && d.currentInput < len(d.inputs) {
				var cmd tea.Cmd
				d.inputs[d.currentInput], cmd = d.inputs[d.currentInput].Update(msg)
				d.ensureInputVisible()
				cmds = append(cmds, cmd)
			}
		}
	}

	return d, tea.Batch(cmds...)
}

func (d *MCPPromptInputDialog) buildBody(innerWidth int) (body string, fieldStarts, fieldHeights []int) {
	var parts []string
	if d.promptInfo.Description != "" {
		parts = append(parts, styles.DialogContentStyle.Width(innerWidth).Render(d.promptInfo.Description), "")
	}
	fieldStarts = make([]int, len(d.inputs))
	fieldHeights = make([]int, len(d.inputs))
	line := 0
	for _, part := range parts {
		line += lipgloss.Height(part)
	}
	for i := range d.inputs {
		label := d.arguments[i].Name
		if d.arguments[i].Required {
			label += " *"
		}
		labelView := styles.DialogContentStyle.Bold(i == d.currentInput).Width(innerWidth).Render(label)
		input := d.inputs[i]
		input.SetStyles(styles.DialogInputStyle)
		input.SetWidth(max(1, innerWidth-lipgloss.Width(input.Prompt)))
		fieldStarts[i] = line
		fieldHeights[i] = lipgloss.Height(labelView) + 1
		parts = append(parts, labelView, input.View())
		line += fieldHeights[i]
		if i < len(d.inputs)-1 {
			parts = append(parts, "")
			line++
		}
	}
	if len(d.inputs) == 0 {
		parts = append(parts, styles.DialogContentStyle.Width(innerWidth).Render("No required parameters"))
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...), fieldStarts, fieldHeights
}

func (d *MCPPromptInputDialog) content() (width int, header, body, footer string) {
	dialogWidth, contentWidth := d.mcpPromptDialogDimensions()
	body, _, _ = d.buildBody(d.BodyContentWidth(dialogWidth))
	header = RenderTitle("MCP Prompt: "+d.promptName, contentWidth, styles.DialogTitleStyle)
	footer = d.RenderActions(contentWidth, Action{Label: "Execute", Default: true, Key: tea.KeyPressMsg{Code: tea.KeyEnter}, Disabled: !d.canExecute()})
	return dialogWidth, header, body, footer
}

func (d *MCPPromptInputDialog) View() string {
	width, header, body, footer := d.content()
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *MCPPromptInputDialog) mcpPromptDialogDimensions() (dialogWidth, contentWidth int) {
	dialogWidth = d.ComputeDialogWidth(80, 60, 120)
	contentWidth = max(1, dialogWidth-styles.DialogStyle.GetHorizontalFrameSize())
	return dialogWidth, contentWidth
}

func (d *MCPPromptInputDialog) ensureInputVisible() {
	if d.currentInput < len(d.fieldStarts) {
		start := d.fieldStarts[d.currentInput]
		d.EnsureBodyLineVisible(start + d.fieldHeights[d.currentInput] - 1)
	}
}

func (d *MCPPromptInputDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
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
	_, _, bodyWidth, bodyHeight := d.BodyScrollBounds()
	if msg.X < d.bodyCol || msg.X >= d.bodyCol+max(1, bodyWidth-2) || msg.Y < d.bodyRow || msg.Y >= d.bodyRow+bodyHeight {
		return d, nil
	}
	line := msg.Y - d.bodyRow + d.BodyScrollOffset()
	for i, start := range d.fieldStarts {
		if line < start || line >= start+d.fieldHeights[i] {
			continue
		}
		d.BlurActions()
		d.inputs[d.currentInput].Blur()
		d.currentInput = i
		cmd := d.inputs[i].Focus()
		if line == start+d.fieldHeights[i]-1 {
			SetTextInputCursorAtCell(&d.inputs[i], msg.X-d.bodyCol-lipgloss.Width(d.inputs[i].Prompt))
		}
		return d, cmd
	}
	return d, nil
}

func (d *MCPPromptInputDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *MCPPromptInputDialog) SetSize(width, height int) tea.Cmd {
	d.reanchorFocus = d.bodyPreparationCount == 0 || width != d.Width() || height != d.Height()
	cmd := d.BaseDialog.SetSize(width, height)
	d.prepareLayout()
	return cmd
}

func (d *MCPPromptInputDialog) prepareLayout() {
	dialogWidth, _ := d.mcpPromptDialogDimensions()
	width := d.BodyContentWidth(dialogWidth)
	for i := range d.inputs {
		d.inputs[i].SetWidth(max(1, width-lipgloss.Width(d.inputs[i].Prompt)))
	}
	_, d.fieldStarts, d.fieldHeights = d.buildBody(width)
	frameWidth, header, body, footer := d.content()
	d.PrepareScrollableBody(styles.DialogStyle, frameWidth, header, body, footer)
	if d.reanchorFocus {
		d.reanchorFocus = false
		d.ensureInputVisible()
		d.PrepareScrollableBody(styles.DialogStyle, frameWidth, header, body, footer)
	}
	d.bodyCol, d.bodyRow, _, _ = d.BodyScrollBounds()
}

func (d *MCPPromptInputDialog) canExecute() bool {
	if d.responseSent {
		return false
	}
	for i, arg := range d.arguments {
		if arg.Required && strings.TrimSpace(d.inputs[i].Value()) == "" {
			return false
		}
	}
	return true
}
