package dialog

import (
	"bytes"
	"fmt"
	stdimage "image"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
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

const maxImagePreviewZoom = 16.0

type imageAttachmentPreviewDialog struct {
	BaseDialog
	title                                string
	preview                              *image.Preview
	cell                                 image.CellSize
	content                              string
	dialogWidth                          int
	maximized                            bool
	viewport                             image.Viewport
	imageX, imageY, imageCols, imageRows int
	panning                              bool
	panStartX, panStartY                 int
	panOriginX, panOriginY               float64
}

func (d *imageAttachmentPreviewDialog) DialogMaximized() bool { return d.maximized }

func (d *imageAttachmentPreviewDialog) Init() tea.Cmd { return nil }
func (d *imageAttachmentPreviewDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)
	d.panning = false
	d.prepare()
	return nil
}

func (d *imageAttachmentPreviewDialog) prepare() {
	if d.preview == nil {
		return
	}
	width, height := max(1, d.width), max(1, d.height)
	if !d.maximized {
		width = min(width, max(40, width*90/100))
		height = min(height, max(12, height*90/100))
	}
	d.dialogWidth = width
	style := styles.DialogStyle
	if d.height < 8 {
		style = style.PaddingTop(0).PaddingBottom(0)
	}
	if width < 10 {
		style = style.PaddingLeft(0).PaddingRight(0)
	}
	inner := max(1, width-style.GetHorizontalFrameSize())
	available := max(1, height-style.GetVerticalFrameSize())
	d.actionRows = nil
	var footer []string
	if inner >= 34 && available >= 6 {
		label := "Maximize"
		if d.maximized {
			label = "Restore"
		}
		actions := d.renderActions(inner, 0, 1, 0,
			Action{Label: "−", Key: tea.KeyPressMsg{Code: '-'}, HideFocusHint: true},
			Action{Label: "+", Key: tea.KeyPressMsg{Code: '+'}, HideFocusHint: true},
			Action{Label: "Fit", Key: tea.KeyPressMsg{Code: '0'}, HideFocusHint: true},
			Action{Label: label, Key: tea.KeyPressMsg{Code: 'm'}, HideFocusHint: true},
		)
		footer = strings.Split(actions, "\n")
	}
	zoom := max(1, d.viewport.Zoom)
	if inner >= 24 && available-len(footer) >= 5 {
		hint := fmt.Sprintf("%.2g× fit · +/- zoom · arrows pan", zoom)
		if inner >= 54 {
			hint = fmt.Sprintf("%.2g× fit · wheel/+/- zoom · drag/arrows pan", zoom)
		}
		// Keep actions last so the shared footer hit testing stays exact.
		footer = append([]string{ansi.Truncate(hint, inner, "")}, footer...)
	}
	var lines []string
	if available-len(footer) >= 3 {
		lines = append(lines, RenderTitle(d.title, inner, styles.DialogTitleStyle), RenderSeparator(inner))
	}
	d.imageX = style.GetBorderLeftSize() + style.GetPaddingLeft()
	d.imageY = style.GetBorderTopSize() + style.GetPaddingTop() + len(lines)
	d.imageCols, d.imageRows = inner, max(0, available-len(lines)-len(footer))
	if d.imageRows > 0 && width >= 7 && height >= 5 {
		markers, err := d.preview.RenderViewport(inner, d.imageRows, d.cell, d.viewport)
		if err != nil {
			lines = append(lines, ansi.Truncate("Image preview unavailable: unable to resize image.", inner, ""))
		} else {
			top := max(0, (d.imageRows-len(markers))/2)
			for range top {
				lines = append(lines, "")
			}
			for _, marker := range markers[:min(len(markers), d.imageRows)] {
				cols, _ := image.MarkerColumns(marker)
				left := max(0, (inner-cols)/2)
				lines = append(lines, strings.Repeat(" ", left)+marker+strings.Repeat(" ", inner-left))
			}
		}
	}
	for len(lines) < available-len(footer) {
		lines = append(lines, "")
	}
	lines = append(lines, footer...)
	d.content = strings.Join(lines, "\n")
}

func (d *imageAttachmentPreviewDialog) imageHit(x, y int) bool {
	row, col := d.Position()
	x, y = x-col-d.imageX, y-row-d.imageY
	return x >= 0 && x < d.imageCols && y >= 0 && y < d.imageRows
}

func (d *imageAttachmentPreviewDialog) setViewport(view image.Viewport) {
	view.Zoom = max(1, min(maxImagePreviewZoom, view.Zoom))
	view.PanX = max(-1, min(1, view.PanX))
	view.PanY = max(-1, min(1, view.PanY))
	if view.Zoom == 1 {
		view.PanX, view.PanY = 0, 0
	}
	if d.viewport == view {
		return
	}
	d.viewport = view
	d.prepare()
	d.MarkVisualDirty()
}

func (d *imageAttachmentPreviewDialog) zoomBy(steps int) {
	if steps != 0 {
		view := d.viewport
		view.Zoom = max(1, view.Zoom) * math.Pow(1.25, float64(max(-12, min(12, steps))))
		d.setViewport(view)
	}
}

func (d *imageAttachmentPreviewDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case ImageCellSizeMsg:
		d.cell = image.CellSize(msg)
		d.prepare()
	case tea.KeyPressMsg:
		view := d.viewport
		switch msg.String() {
		case "esc", "enter", "q":
			return d, d.CancelDialogCmd()
		case "m", "f11":
			d.maximized = !d.maximized
			d.panning = false
			d.prepare()
			d.MarkVisualDirty()
		case "+", "=":
			d.zoomBy(1)
		case "-", "_":
			d.zoomBy(-1)
		case "0", "home":
			d.setViewport(image.Viewport{Zoom: 1})
		case "left", "h":
			view.PanX -= 0.15
			d.setViewport(view)
		case "right", "l":
			view.PanX += 0.15
			d.setViewport(view)
		case "up", "k":
			view.PanY -= 0.15
			d.setViewport(view)
		case "down", "j":
			view.PanY += 0.15
			d.setViewport(view)
		}
	case tea.MouseWheelMsg:
		if d.imageHit(msg.X, msg.Y) {
			switch msg.Button {
			case tea.MouseWheelUp:
				d.zoomBy(1)
			case tea.MouseWheelDown:
				d.zoomBy(-1)
			}
		}
	case messages.WheelCoalescedMsg:
		if d.imageHit(msg.X, msg.Y) {
			d.zoomBy(-msg.Delta)
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && d.imageHit(msg.X, msg.Y) && d.viewport.Zoom > 1 {
			d.panning = true
			d.panStartX, d.panStartY = msg.X, msg.Y
			d.panOriginX, d.panOriginY = d.viewport.PanX, d.viewport.PanY
		}
	case tea.MouseMotionMsg:
		if msg.Button != tea.MouseLeft {
			d.panning = false
		}
		if d.panning {
			view := d.viewport
			view.PanX = d.panOriginX - 2*float64(msg.X-d.panStartX)/float64(max(1, d.imageCols))
			view.PanY = d.panOriginY - 2*float64(msg.Y-d.panStartY)/float64(max(1, d.imageRows))
			d.setViewport(view)
		}
	case tea.MouseReleaseMsg:
		d.panning = false
	}
	return d, nil
}
func (d *imageAttachmentPreviewDialog) View() string {
	if d.width <= 0 || d.height <= 0 {
		return ""
	}
	return d.RenderCard(styles.DialogStyle, d.dialogWidth, d.content)
}
func (d *imageAttachmentPreviewDialog) Position() (int, int) { return d.CenterDialog(d.View()) }
func (d *imageAttachmentPreviewDialog) Cleanup() {
	if d.preview != nil {
		d.preview.Close()
		d.preview = nil
	}
	d.content = ""
	d.panning = false
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

// NewUserAttachmentPreviewDialog previews only bytes/text already present in the
// session projection. It never resolves SourcePath or fetches remote content.
func NewUserAttachmentPreviewDialog(ar *animation.Runtime, attachment types.UserAttachment, supported bool) Dialog {
	const previewLimit = 20 << 20
	title := attachment.Name
	if title == "" {
		title = "Attachment"
	}
	if attachment.SourcePath != "" {
		title += " · " + attachment.SourcePath
	}
	if !attachment.Available {
		reason := attachment.UnavailableReason
		if reason == "" {
			reason = "stored content unavailable"
		}
		return NewAttachmentPreviewDialog(ar, title, "Attachment preview unavailable: "+reason+".")
	}
	if strings.HasPrefix(attachment.MediaType, "image/") {
		if len(attachment.Data) == 0 {
			return NewAttachmentPreviewDialog(ar, title, "Image preview unavailable: stored image bytes are unavailable.")
		}
		return NewImageAttachmentPreviewDialog(ar, title, attachment.MediaType, attachment.Data, supported)
	}
	if attachment.Text != "" || strings.HasPrefix(attachment.MediaType, "text/") {
		text := attachment.Text
		if text == "" {
			if !utf8.Valid(attachment.Data) {
				return NewAttachmentPreviewDialog(ar, title, "Text preview unavailable: stored bytes are not valid UTF-8 text.")
			}
			text = string(attachment.Data)
		}
		if len(text) > previewLimit {
			return NewAttachmentPreviewDialog(ar, title, "Text preview unavailable: attachment exceeds the 20 MiB preview limit.")
		}
		if attachment.SourcePath != "" {
			text = "Source: " + attachment.SourcePath + "\n\n" + text
		}
		if len(text) > previewLimit {
			return NewAttachmentPreviewDialog(ar, title, "Text preview unavailable: attachment exceeds the 20 MiB preview limit.")
		}
		return NewAttachmentPreviewDialog(ar, title, text)
	}
	if len(attachment.Data) > previewLimit {
		return NewAttachmentPreviewDialog(ar, title, "Attachment preview unavailable: file exceeds the 20 MiB preview limit.")
	}
	if len(attachment.Data) > 0 {
		return NewAttachmentPreviewDialog(ar, title, fmt.Sprintf("Preview unavailable for %s (%d bytes). Stored file content is not rendered as text.", attachment.MediaType, len(attachment.Data)))
	}
	return NewAttachmentPreviewDialog(ar, title, "Preview unavailable for this attachment type.")
}
