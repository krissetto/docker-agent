package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// attachmentPreviewDialog uses the same read-only viewport and lifecycle as help.
type attachmentPreviewDialog struct{ readOnlyScrollDialog }

// NewAttachmentPreviewDialog displays already-loaded attachment text without I/O.
// The manager owns the shared runtime; this content has no independent animation.
func NewAttachmentPreviewDialog(_ *animation.Runtime, title, content string) Dialog {
	d := &attachmentPreviewDialog{}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(readOnlyScrollDialogSize{
		widthPercent: 80, minWidth: 40, maxWidth: 140, heightPercent: 80, heightMax: 60,
	}, func(width, _ int) []string {
		lines := []string{RenderTitle(title, width, styles.DialogTitleStyle), RenderSeparator(width), ""}
		for line := range strings.SplitSeq(content, "\n") {
			lines = append(lines, strings.Split(ansi.Hardwrap(line, max(1, width), true), "\n")...)
		}
		return lines
	})
	return d
}

func (d *attachmentPreviewDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := d.readOnlyScrollDialog.Update(msg)
	return d, cmd
}

func (d *attachmentPreviewDialog) Cleanup() {}
