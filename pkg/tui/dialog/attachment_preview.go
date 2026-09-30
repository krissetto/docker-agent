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
func NewImageAttachmentPreviewDialog(ar *animation.Runtime, title, mimeType string, data []byte, supported bool) Dialog {
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
	unavailable := "Image preview unavailable: this terminal does not support image rendering."
	if !supported {
		return NewAttachmentPreviewDialog(ar, title, info+"\n\n"+unavailable)
	}
	preview, err := image.DecodePreview(title, data)
	if err != nil {
		return NewAttachmentPreviewDialog(ar, title, info+"\n\nImage preview unavailable: unable to decode image data.")
	}
	return &imageAttachmentPreviewDialog{title: title, preview: preview}
}

// NewCachedImagePreviewDialog takes ownership of a click's existing source lease.
func NewCachedImagePreviewDialog(ar *animation.Runtime, preview *image.Preview, supported bool) Dialog {
	if preview == nil {
		return NewAttachmentPreviewDialog(ar, "Image preview", "Image preview unavailable.")
	}
	title := safeAttachmentText(preview.Name())
	if title == "" {
		title = "Image preview"
	}
	if !supported {
		preview.Close()
		return NewAttachmentPreviewDialog(ar, title, "Image preview unavailable: this terminal does not support image rendering.")
	}
	return &imageAttachmentPreviewDialog{title: title, preview: preview}
}

type ImageCellSizeMsg image.CellSize

type imageAttachmentPreviewDialog struct {
	BaseDialog
	title       string
	preview     *image.Preview
	cell        image.CellSize
	content     string
	dialogWidth int
}

func (d *imageAttachmentPreviewDialog) Init() tea.Cmd { return nil }
func (d *imageAttachmentPreviewDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	d.prepare()
	return nil
}
func (d *imageAttachmentPreviewDialog) prepare() {
	cell := d.cell.Resolved()
	cols, rows := max(1, d.width-6), max(1, d.height-6)
	img, err := d.preview.Fit(cols, rows, cell)
	if err != nil {
		d.content = "Image preview unavailable: unable to resize image."
		return
	}
	placementCols := (img.Width + cell.Width - 1) / cell.Width
	d.dialogWidth = min(d.width, max(placementCols, min(ansi.StringWidth(d.title), cols))+6)
	inner := max(1, d.dialogWidth-6)
	lines := []string{RenderTitle(d.title, inner, styles.DialogTitleStyle), RenderSeparator(inner)}
	if d.width >= 7 && d.height >= 7 {
		for _, marker := range image.RenderNativePreviewMarkers(img, cell) {
			left := max(0, (inner-placementCols)/2)
			lines = append(lines, strings.Repeat(" ", left)+marker+strings.Repeat(" ", inner-left))
		}
	}
	d.content = strings.Join(lines, "\n")
}
func (d *imageAttachmentPreviewDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case ImageCellSizeMsg:
		d.cell = image.CellSize(msg)
		d.prepare()
	case tea.KeyPressMsg:
		if msg.Code == tea.KeyEscape || msg.Code == tea.KeyEnter || msg.String() == "q" {
			return d, d.CancelDialogCmd()
		}
	}
	return d, nil
}
func (d *imageAttachmentPreviewDialog) View() string {
	return d.RenderCard(styles.DialogStyle, d.dialogWidth, d.content)
}
func (d *imageAttachmentPreviewDialog) Position() (int, int) { return d.CenterDialog(d.View()) }
func (d *imageAttachmentPreviewDialog) Cleanup()             { d.preview.Close(); d.content = "" }

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
