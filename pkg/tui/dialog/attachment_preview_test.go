package dialog

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	stdimage "image"
	"image/png"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentPreviewUsesSharedViewportAndPreservesWhitespace(t *testing.T) {
	d := NewAttachmentPreviewDialog(newDialogRuntime(), "file.txt", "    indented\n"+strings.Repeat("line\n", 40)).(*attachmentPreviewDialog)
	d.SetSize(80, 24)
	assert.Contains(t, ansi.Strip(d.View()), "    indented")
	require.True(t, d.scrollview.NeedsScrollbar())
	updated, _ := d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	assert.Same(t, d, updated)
	assert.Positive(t, d.scrollview.ScrollOffset())
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	assert.IsType(t, CloseDialogMsg{}, cmd())
	CleanupDialog(d)
}

func previewPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, stdimage.NewRGBA(stdimage.Rect(0, 0, 40, 80))))
	return buf.Bytes()
}

func TestImageAttachmentPreviewAvailabilityAndBounds(t *testing.T) {
	data := previewPNG(t)
	for _, enabled := range []bool{false, true} {
		d := NewImageAttachmentPreviewDialog(newDialogRuntime(), "photo.png", "image/png", data, enabled)
		for _, size := range [][2]int{{80, 24}, {30, 12}, {8, 4}, {80, 24}} {
			d.SetSize(size[0], size[1])
			view := d.View()
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			assert.NotContains(t, view, "\x1b_G", "only deferred markers, never raw terminal graphics")
			if size[0] == 80 {
				if enabled {
					assert.NotContains(t, ansi.Strip(view), "pixels")
				} else {
					assert.Contains(t, ansi.Strip(view), "40 × 80 pixels")
				}
				if enabled {
					assert.Contains(t, view, "cagent-image;")
				} else {
					assert.Contains(t, ansi.Strip(view), "does not")
					assert.NotContains(t, view, "cagent-image;")
				}
			}
		}

	}
	bad := NewImageAttachmentPreviewDialog(newDialogRuntime(), "bad.png", "image/png", []byte("\x1b_Gjunk"), true)
	bad.SetSize(100, 30)
	assert.Contains(t, ansi.Strip(bad.View()), "invalid or unsupported")
	assert.NotContains(t, bad.View(), "\x1b_G")
}

func TestImageAttachmentMarkersOnlyAppearInSettledLifecycle(t *testing.T) {
	r := newDialogRuntime()
	d := NewImageAttachmentPreviewDialog(r, "photo.png", "image/png", previewPNG(t), true)
	d.SetSize(80, 30)
	a, _ := newAnimatedDialog(r, d, 80, 30)
	assert.NotContains(t, a.view(), "cagent-image;")
	advanceDialog(r, r.Continue(), dialogOpenDuration)
	a.tick("", 80, 30)
	require.Contains(t, a.view(), "cagent-image;")
	d.SetSize(10, 7)
	a.retarget("resize", 10, 7)
	assert.NotContains(t, a.view(), "cagent-image;")
	advanceDialog(r, r.Continue(), dialogOpenDuration+dialogResizeDuration)
	a.tick("", 10, 7)
	require.Contains(t, a.view(), "cagent-image;")
	a.startClose(false)
	assert.NotContains(t, a.view(), "cagent-image;")
	a.cancel()
	CleanupDialog(d)
	other := NewImageAttachmentPreviewDialog(r, "other.png", "image/png", previewPNG(t), true)
	other.SetSize(80, 30)
	assert.Contains(t, other.View(), "cagent-image;", "cleanup never clears shared registry or rendering preference")
}

func TestAttachmentTextCannotInjectTerminalSequences(t *testing.T) {
	d := NewAttachmentPreviewDialog(newDialogRuntime(), "bad\x1btitle", "text\x1b_Ga=T;data\x1b\\\x00end")
	d.SetSize(80, 24)
	assert.NotContains(t, d.View(), "\x1b_G")
	assert.NotContains(t, d.View(), "\x00")
	assert.Contains(t, ansi.Strip(d.View()), "text�_G")
}

func TestImageAttachmentPreviewRejectsPixelBombBeforeDecode(t *testing.T) {
	data := previewPNG(t)
	binary.BigEndian.PutUint32(data[16:20], 100_000)
	binary.BigEndian.PutUint32(data[20:24], 100_000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	d := NewImageAttachmentPreviewDialog(newDialogRuntime(), "large.png", "image/png", data, true)
	d.SetSize(100, 30)
	assert.Contains(t, ansi.Strip(d.View()), "16 megapixel")
	assert.NotContains(t, d.View(), "cagent-image;")
}

func TestNativeImageDialogCentersActualPlacementAndUsesAvailableScreen(t *testing.T) {
	for _, pixels := range [][2]int{{1, 1}, {1600, 100}, {100, 1600}, {1800, 1200}} {
		var data bytes.Buffer
		require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, pixels[0], pixels[1]))))
		d := NewImageAttachmentPreviewDialog(nil, "picture.png", "image/png", data.Bytes(), true).(*imageAttachmentPreviewDialog)
		for _, size := range [][2]int{{180, 60}, {40, 12}, {10, 6}, {180, 60}} {
			d.Update(ImageCellSizeMsg{Width: 9, Height: 23})
			d.SetSize(size[0], size[1])
			view := d.View()
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			assert.NotContains(t, ansi.Strip(view), "pixels")
			assert.NotContains(t, ansi.Strip(view), "image/png")
			for _, line := range strings.Split(view, "\n") {
				at := strings.Index(line, "\x1b_cagent-image;")
				if at < 0 {
					continue
				}
				stop := strings.Index(line[at:], "\x1b\\") + at + 2
				cols, ok := image.MarkerColumns(line[at:stop])
				require.True(t, ok)
				x := ansi.StringWidth(line[:at])
				assert.InDelta(t, x, lipgloss.Width(view)-x-cols, 1, "graphic extent, not zero-width marker text, is centered")
			}
		}
	}
}

func TestUserAttachmentPreviewUsesOnlyStoredPayloadAndBounds(t *testing.T) {
	text := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
		Name: "notes 世界.txt", MediaType: "text/plain", SourcePath: "/deleted/notes.txt", Text: "stored text", Available: true,
	}, false)
	text.SetSize(90, 28)
	plain := ansi.Strip(text.View())
	assert.Contains(t, plain, "stored text")
	assert.Contains(t, plain, "/deleted/notes.txt", "source path is provenance only")
	CleanupDialog(text)

	empty := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
		Name: "empty.txt", MediaType: "text/plain", Available: true,
	}, false)
	empty.SetSize(90, 28)
	assert.NotContains(t, ansi.Strip(empty.View()), "Preview unavailable")
	CleanupDialog(empty)

	unavailable := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
		Name: "old.pdf", MediaType: "application/pdf", SourcePath: "/must/not/read.pdf", Available: false, UnavailableReason: "legacy attachment content unavailable",
	}, false)
	unavailable.SetSize(90, 28)
	assert.Contains(t, ansi.Strip(unavailable.View()), "legacy attachment content unavai")
	assert.Contains(t, ansi.Strip(unavailable.View()), "lable.")
	assert.NotContains(t, ansi.Strip(unavailable.View()), "must not read")
	CleanupDialog(unavailable)

	oversized := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
		Name: "large.txt", MediaType: "text/plain", Text: strings.Repeat("x", (20<<20)+1), Available: true,
	}, false)
	oversized.SetSize(90, 28)
	assert.Contains(t, ansi.Strip(oversized.View()), "20 MiB")
	CleanupDialog(oversized)
}

func TestUserImageAttachmentPreviewHonorsGraphicsAndCorruptData(t *testing.T) {
	data := previewPNG(t)
	for _, enabled := range []bool{false, true} {
		d := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
			Name: "inline.png", MediaType: "image/png", Data: data, Available: true,
		}, enabled)
		d.SetSize(90, 28)
		if enabled {
			assert.Contains(t, d.View(), "cagent-image;")
		} else {
			assert.Contains(t, ansi.Strip(d.View()), "does not support")
		}
		CleanupDialog(d)
	}
	bad := NewUserAttachmentPreviewDialog(newDialogRuntime(), types.UserAttachment{
		Name: "bad.png", MediaType: "image/png", Data: []byte("not an image"), Available: true,
	}, true)
	bad.SetSize(90, 28)
	assert.Contains(t, ansi.Strip(bad.View()), "invalid or unsupported")
	CleanupDialog(bad)
}

func TestImagePreviewZoomPanMaximizeAndRestore(t *testing.T) {
	d := NewImageAttachmentPreviewDialog(nil, "photo.png", "image/png", previewPNG(t), true).(*imageAttachmentPreviewDialog)
	t.Cleanup(d.Cleanup)
	d.SetSize(100, 40)
	initialWidth, initialHeight := lipgloss.Width(d.View()), lipgloss.Height(d.View())
	assert.Equal(t, 90, initialWidth)
	assert.Equal(t, 36, initialHeight)
	assert.Contains(t, ansi.Strip(d.View()), "Maximize")
	assert.Contains(t, ansi.Strip(d.View()), "wheel")

	d.Update(tea.KeyPressMsg{Code: '+'})
	assert.Greater(t, d.viewport.Zoom, 1.0)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Positive(t, d.viewport.PanX)
	assert.Positive(t, d.viewport.PanY)
	assert.Equal(t, initialWidth, lipgloss.Width(d.View()))
	assert.Equal(t, initialHeight, lipgloss.Height(d.View()), "zoom and pan do not resize the card")

	beforeMaximize := d.viewport
	d.Update(tea.KeyPressMsg{Code: 'm'})
	assert.True(t, d.DialogMaximized())
	assert.Equal(t, 100, lipgloss.Width(d.View()))
	assert.Equal(t, 40, lipgloss.Height(d.View()))
	assert.Contains(t, ansi.Strip(d.View()), "Restore")
	assert.Equal(t, beforeMaximize, d.viewport)
	d.Update(tea.KeyPressMsg{Code: 'm'})
	assert.False(t, d.DialogMaximized())
	assert.Equal(t, initialWidth, lipgloss.Width(d.View()))
	d.Update(tea.KeyPressMsg{Code: '0'})
	assert.Equal(t, image.Viewport{Zoom: 1}, d.viewport)
	for range 100 {
		d.Update(tea.KeyPressMsg{Code: '+'})
		d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	assert.Equal(t, maxImagePreviewZoom, d.viewport.Zoom)
	assert.Equal(t, 1.0, d.viewport.PanX)
}

func TestImagePreviewMouseControlsStayInsideCanvasAndRequireHeldDrag(t *testing.T) {
	d := NewImageAttachmentPreviewDialog(nil, "photo.png", "image/png", previewPNG(t), true).(*imageAttachmentPreviewDialog)
	t.Cleanup(d.Cleanup)
	d.SetSize(100, 40)
	row, col := d.Position()
	x, y := col+d.imageX+d.imageCols/2, row+d.imageY+d.imageRows/2
	outside := d.viewport
	d.Update(tea.MouseWheelMsg{X: 0, Y: 0, Button: tea.MouseWheelUp})
	assert.Equal(t, outside, d.viewport)
	d.Update(messages.WheelCoalescedMsg{X: x, Y: row, Delta: -2})
	assert.Equal(t, outside, d.viewport, "title is not the zoom target")
	d.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	assert.Greater(t, d.viewport.Zoom, 1.0)
	assert.True(t, d.TakeVisualDirty())
	d.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: -2})
	assert.Greater(t, d.viewport.Zoom, 1.25)

	d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.Update(tea.MouseMotionMsg{X: x - 3, Y: y - 2, Button: tea.MouseLeft})
	assert.Positive(t, d.viewport.PanX)
	assert.Positive(t, d.viewport.PanY)
	afterDrag := d.viewport
	d.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.Update(tea.MouseMotionMsg{X: x + 5, Y: y + 5, Button: tea.MouseLeft})
	assert.Equal(t, afterDrag, d.viewport)
	d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.Update(tea.MouseMotionMsg{X: x + 5, Y: y + 5})
	assert.False(t, d.panning)
	assert.Equal(t, afterDrag, d.viewport)
}

func TestImagePreviewFooterHitboxesAndResizeBounds(t *testing.T) {
	d := NewImageAttachmentPreviewDialog(nil, "photo.png", "image/png", previewPNG(t), true).(*imageAttachmentPreviewDialog)
	t.Cleanup(d.Cleanup)
	d.SetSize(100, 40)
	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	found := map[rune]bool{}
	for y := row; y < row+dl.Height; y++ {
		for x := col; x < col+dl.Width; x++ {
			if key, hit := d.ActionKeyAt(x, y, dl); hit {
				found[key.Code] = true
				assert.False(t, d.imageHit(x, y), "footer buttons never overlap image")
			}
		}
	}
	for _, code := range []rune{'-', '+', '0', 'm'} {
		assert.True(t, found[code], "control %q is clickable", code)
	}
	d.Update(tea.KeyPressMsg{Code: '+'})
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	state := d.viewport
	for _, maximized := range []bool{false, true} {
		d.maximized = maximized
		for _, size := range [][2]int{{160, 50}, {40, 12}, {10, 7}, {8, 4}, {1, 1}, {100, 40}} {
			d.SetSize(size[0], size[1])
			assert.LessOrEqual(t, lipgloss.Width(d.View()), size[0])
			assert.LessOrEqual(t, lipgloss.Height(d.View()), size[1])
			assert.Equal(t, state, d.viewport, "terminal resize preserves image navigation")
		}
	}
	d.Cleanup()
	d.Cleanup()
	assert.Nil(t, d.preview)
}

func TestImagePreviewManagerMaximizeClearsDragOffset(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	t.Cleanup(mgr.Cleanup)
	mgr.SetSize(100, 40)
	d := NewImageAttachmentPreviewDialog(nil, "photo.png", "image/png", previewPNG(t), true).(*imageAttachmentPreviewDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))
	mgr.stack[0].offsetX, mgr.stack[0].offsetY = 3, 2
	mgr.Update(tea.KeyPressMsg{Code: 'm'})
	assert.Zero(t, mgr.stack[0].offsetX)
	assert.Zero(t, mgr.stack[0].offsetY)
	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogResizeDuration+dialogOpenDuration))
	row, col := mgr.stack[0].position(100, 40)
	assert.False(t, mgr.handleDragStart(col+5, row+1), "maximized title cannot move card offscreen")
	mgr.Update(tea.KeyPressMsg{Code: 'm'})
	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogResizeDuration+dialogOpenDuration))
	row, col = mgr.stack[0].position(100, 40)
	assert.True(t, mgr.handleDragStart(col+5, row+1), "restored card retains ordinary title dragging")
}
