package dialog

import (
	"bytes"
	"fmt"
	stdimage "image"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// attachmentPreviewDialog uses the same read-only viewport and lifecycle as help.
type attachmentPreviewDialog struct{ readOnlyScrollDialog }

// NewAttachmentPreviewDialog displays already-loaded attachment text without I/O.
// The manager owns the shared runtime; this content has no independent animation.
func NewAttachmentPreviewDialog(_ *animation.Runtime, title, content string) Dialog {
	title = safeAttachmentText(title)
	content = safeAttachmentText(content)
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

// NewImageAttachmentPreviewDialog decodes once, before the shared render lifecycle.
func NewImageAttachmentPreviewDialog(ar *animation.Runtime, title, mimeType string, data []byte, enabled bool) Dialog {
	title = safeAttachmentText(title)
	const maxPixels = 16_000_000
	if len(data) > 20<<20 {
		return NewAttachmentPreviewDialog(ar, title, "Image preview unavailable: file exceeds the 20 MiB preview limit.")
	}
	config, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return NewAttachmentPreviewDialog(ar, title, "Image preview unavailable: invalid or unsupported image data.")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxPixels/config.Height {
		return NewAttachmentPreviewDialog(ar, title, "Image preview unavailable: image exceeds the 16 megapixel preview limit.")
	}
	mimeType = "image/" + format
	info := fmt.Sprintf("%s · %d × %d pixels", mimeType, config.Width, config.Height)
	unavailable := "Image preview unavailable: image rendering is disabled or unsupported by this terminal."
	if !enabled {
		return NewAttachmentPreviewDialog(ar, title, info+"\n\n"+unavailable)
	}
	img, ok := image.FromBytes(title, mimeType, data)
	if !ok {
		return NewAttachmentPreviewDialog(ar, title, info+"\n\nImage preview unavailable: unable to decode image data.")
	}
	d := &attachmentPreviewDialog{}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(readOnlyScrollDialogSize{
		widthPercent: 80, minWidth: 40, maxWidth: 140, heightPercent: 80, heightMax: 60,
	}, func(width, _ int) []string {
		lines := []string{RenderTitle(title, width, styles.DialogTitleStyle), RenderSeparator(width), ""}
		lines = append(lines, strings.Split(ansi.Hardwrap(info, width, true), "\n")...)
		lines = append(lines, "")
		markers := image.RenderMarkers(img, width)
		if len(markers) == 0 || width < 4 {
			return append(lines, strings.Split(ansi.Hardwrap(unavailable, width, true), "\n")...)
		}
		for _, marker := range markers {
			lines = append(lines, marker+strings.Repeat(" ", max(0, width-ansi.StringWidth(marker))))
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

func safeAttachmentText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return '�'
		}
		return r
	}, text)
}
