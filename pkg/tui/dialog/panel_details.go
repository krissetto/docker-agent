package dialog

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// panelDetailsDialog presents a detached, read-only snapshot. It never queries
// live session state or performs workspace/session mutations.
type panelDetailsDialog struct {
	readOnlyScrollDialog
	title string
	lines []string
}

// NewPanelDetailsDialog creates a scrollable inspector from copied plain-text
// lines. The caller chooses the snapshot and supplies an informative empty state.
func NewPanelDetailsDialog(title string, lines []string) Dialog {
	d := &panelDetailsDialog{title: title, lines: slices.Clone(lines)}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{widthPercent: 70, minWidth: 40, maxWidth: 100, heightPercent: 80, heightMax: 40},
		d.renderLines,
	)
	return d
}

func (d *panelDetailsDialog) renderLines(width, _ int) []string {
	width = max(1, width)
	title := ansi.Truncate(ansi.Strip(strings.ReplaceAll(d.title, "\n", " ")), width, "…")
	lines := []string{RenderTitle(title, width, styles.DialogTitleStyle), RenderSeparator(width), ""}
	if len(d.lines) == 0 {
		lines = append(lines, "No details available.")
	}
	for _, line := range d.lines {
		// Wrap, rather than byte-truncate, so long paths and Unicode todo text
		// remain inspectable on narrow terminals without spilling the modal.
		lines = append(lines, strings.Split(ansi.Hardwrap(ansi.Strip(line), width, true), "\n")...)
	}
	return lines
}
