package dialog

import (
	"github.com/charmbracelet/x/ansi"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// readOnlyScrollDialogSize defines the sizing parameters for a read-only scroll dialog.
type readOnlyScrollDialogSize struct {
	widthPercent  int
	minWidth      int
	maxWidth      int
	heightPercent int
	heightMax     int
}

// contentRenderer renders dialog content lines given the available width and max height.
type contentRenderer func(contentWidth, maxHeight int) []string

// readOnlyScrollDialog is a base for simple read-only dialogs with scrollable content.
// It handles Init, Update (scrollview + close key), Position, View, and scrolling.
// Concrete dialogs embed it and provide a contentRenderer and help key bindings.
type readOnlyScrollDialog struct {
	BaseDialog

	scrollview *scrollview.Model
	closeKey   key.Binding
	size       readOnlyScrollDialogSize
	render     contentRenderer
	helpKeys   []string // pairs of [key, description] for the footer
}

// Dialog chrome: border (top+bottom=2) + padding (top+bottom=2).
const dialogChrome = 4

// Fixed lines outside the scrollable region: header (title + separator + space) + footer (space + help).
const fixedLines = 5

// newReadOnlyScrollDialog creates a new read-only scrollable dialog.
func newReadOnlyScrollDialog(
	size readOnlyScrollDialogSize,
	render contentRenderer,
) readOnlyScrollDialog {
	base := BaseDialog{bodyCompactTitle: true}
	scrollviewView := base.newScrollview(
		scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
		scrollview.WithReserveScrollbarSpace(true),
	)
	base.bodyScroll = scrollviewView
	return readOnlyScrollDialog{
		BaseDialog: base,
		scrollview: scrollviewView,
		closeKey:   key.NewBinding(key.WithKeys("esc", "enter", "q")),
		size:       size,
		render:     render,
		helpKeys:   []string{"↑↓", "scroll"},
	}
}

func (d *readOnlyScrollDialog) Init() tea.Cmd {
	return nil
}

func (d *readOnlyScrollDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if preparesDialogBody(msg) {
		defer d.renderBody(true)
	}
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if key.Matches(msg, d.closeKey) {
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}
	return d, nil
}

func (d *readOnlyScrollDialog) dialogWidth() (dialogWidth, contentWidth int) {
	s := d.size
	dialogWidth = d.ComputeDialogWidth(s.widthPercent, s.minWidth, s.maxWidth)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, contentWidth
}

// maxViewport returns the maximum number of scrollable lines that fit.
func (d *readOnlyScrollDialog) maxViewport() int {
	s := d.size
	maxHeight := min(d.Height()*s.heightPercent/100, s.heightMax)
	return max(1, maxHeight-fixedLines-dialogChrome)
}

func (d *readOnlyScrollDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *readOnlyScrollDialog) View() string { return d.renderBody(false) }

func (d *readOnlyScrollDialog) renderBody(prepare bool) string {
	dialogWidth, contentWidth := d.dialogWidth()
	allLines := d.render(max(1, contentWidth), d.maxViewport())
	headerLines := min(3, len(allLines))
	footer := ""
	if len(allLines) < 3 || strings.TrimSpace(ansi.Strip(allLines[2])) == "" {
		footer = styles.MutedStyle.Render(ansi.Wrap("↑/↓ scroll · PgUp/PgDn page · Esc close", max(1, contentWidth), ""))
	}
	if prepare {
		d.bodyMaxHeight = max(1, min(d.Height()*d.size.heightPercent/100, d.size.heightMax))
		d.PrepareScrollableBody(styles.DialogStyle, dialogWidth, strings.Join(allLines[:headerLines], "\n"), strings.Join(allLines[headerLines:], "\n"), footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, dialogWidth, strings.Join(allLines[:headerLines], "\n"), strings.Join(allLines[headerLines:], "\n"), footer)
}

func (d *readOnlyScrollDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	return cmd
}
