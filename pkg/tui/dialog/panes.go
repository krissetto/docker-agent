package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/textinput"
)

// panesDialog is the local action/source/divider chooser for /panes. The root
// owns routing identities and transactions; this view only displays the short,
// optionally agent-colored labels it supplies and invokes the chosen callback.
type panesDialog struct {
	pickerCore

	title    string
	items    []commands.Item
	filtered []commands.Item
}

// NewPanesDialog builds a compact, searchable pane chooser. An empty title uses
// "Panes"; callers may name a source or divider selection with a short subtitle.
// Labels and descriptions are display text, never routing keys. Execute receives
// an empty argument, just as it does for a command chosen from the palette.
func NewPanesDialog(title string, items []commands.Item) Dialog {
	if strings.TrimSpace(title) == "" {
		title = "Panes"
	}
	d := &panesDialog{
		pickerCore: newPickerCore(pickerLayout{
			WidthPercent: 60, MinWidth: 32, MaxWidth: 76,
			HeightPercent: 70, MaxHeight: 24, ListOverhead: 7,
		}, "Filter panes…"),
		title: title,
		items: append([]commands.Item(nil), items...),
	}
	d.filterItems()
	return d
}

func (d *panesDialog) Init() tea.Cmd { return textinput.Blink }

// CancelDialogCmd makes Escape, the close control and manager dismissal share
// one terminal result. Cancellation never invokes an item callback.
func (d *panesDialog) CancelDialogCmd() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return closeDialogCmd()
}

func (d *panesDialog) executeSelected() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) || d.filtered[d.selected].Execute == nil || !d.claimResponse() {
		return nil
	}
	return tea.Sequence(closeDialogCmd(), d.filtered[d.selected].Execute(""))
}

func (d *panesDialog) filterItems() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	d.filtered = d.filtered[:0]
	for _, item := range d.items {
		text := strings.ToLower(ansi.Strip(item.Label + " " + item.Description))
		if query == "" || strings.Contains(text, query) {
			d.filtered = append(d.filtered, item)
		}
	}
	d.selected = 0
	d.lastClickIndex = -1
	d.scrollview.SetScrollOffset(0)
}

func (d *panesDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if d.responseSent {
		return d, nil
	}
	if preparesDialogBody(msg) {
		defer d.renderBody(true)
	}
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
		view := d.View()
		row, col := d.Position()
		if d.CloseButtonHit(click, NewDialogLayout(view, row, col)) {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.Code {
		case tea.KeyEscape:
			cmd := d.CancelDialogCmd()
			return d, cmd
		case tea.KeyUp:
			d.navigate(-1, len(d.filtered), nil)
			return d, nil
		case tea.KeyDown:
			d.navigate(1, len(d.filtered), nil)
			return d, nil
		case tea.KeyEnter:
			cmd := d.executeSelected()
			return d, cmd
		}
		if key.String() == "ctrl+c" {
			cmd := d.CancelDialogCmd()
			return d, cmd
		}
	}
	// Keep printable keys (including j/k/q) exclusively in the filter rather
	// than forwarding them to the scrollview's optional navigation aliases.
	_, typing := msg.(tea.KeyPressMsg)
	if !typing {
		if handled, cmd := d.scrollview.Update(msg); handled {
			return d, cmd
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.MouseClickMsg:
		if twice, _ := d.handleListClick(msg, func(line int) int {
			if line < 0 || line >= len(d.filtered) {
				return -1
			}
			return line
		}); twice {
			cmd := d.executeSelected()
			return d, cmd
		}
	case tea.KeyPressMsg, tea.PasteMsg:
		cmd := d.updateInput(msg, d.filterItems)
		return d, cmd
	default:
		cmd := d.updateInput(msg, nil)
		return d, cmd
	}
	return d, nil
}

func (d *panesDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.pickerCore.SetSize(width, height)
	d.renderBody(true)
	return cmd
}

func (d *panesDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *panesDialog) View() string { return d.renderBody(false) }

func (d *panesDialog) renderBody(prepare bool) string {
	width, _, inner := d.dialogSize()
	input := d.textInput
	input.SetStyles(styles.DialogInputStyle)
	input.SetWidth(inner)
	if prepare {
		d.textInput = input
	}
	header := RenderTitle(d.title, inner, styles.DialogTitleStyle) + "\n" + input.View()
	lines := make([]string, 0, len(d.filtered))
	for index, item := range d.filtered {
		prefix := "  "
		if index == d.selected {
			prefix = styles.HighlightWhiteStyle.Render("› ")
		}
		// Keep root-provided agent colors intact, even in the selected row.
		line := prefix + item.Label
		if item.Description != "" {
			line += styles.MutedStyle.Render(" · " + item.Description)
		}
		lines = append(lines, ansi.Truncate(line, inner, "…"))
	}
	if len(lines) == 0 {
		lines = []string{styles.MutedStyle.Render("No matching panes")}
	}
	footer := styles.MutedStyle.Render(ansi.Truncate("↑/↓ choose · Enter/double-click apply · Esc cancel", inner, ""))
	return d.renderPicker(prepare, width, header, lines, footer)
}
