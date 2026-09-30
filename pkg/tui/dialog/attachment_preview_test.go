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

func TestImageAttachmentPreviewAvailabilityBoundsAndScroll(t *testing.T) {
	data := previewPNG(t)
	for _, enabled := range []bool{false, true} {
		d := NewImageAttachmentPreviewDialog(newDialogRuntime(), "photo.png", "image/png", data, enabled).(*attachmentPreviewDialog)
		for _, size := range [][2]int{{80, 24}, {30, 12}, {8, 4}, {80, 24}} {
			d.SetSize(size[0], size[1])
			view := d.View()
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			assert.NotContains(t, view, "\x1b_G", "only deferred markers, never raw terminal graphics")
			if size[0] == 80 {
				assert.Contains(t, ansi.Strip(view), "40 × 80 pixels")
				if enabled {
					assert.Contains(t, view, "cagent-image;")
				} else {
					assert.Contains(t, ansi.Strip(view), "does not")
					assert.NotContains(t, view, "cagent-image;")
				}
			}
		}
		if enabled {
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
			assert.Nil(t, cmd)
			assert.Positive(t, d.BodyScrollOffset())
			assert.Contains(t, d.View(), "cagent-image;")
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
	d.SetSize(40, 12)
	a.retarget("resize", 40, 12)
	assert.NotContains(t, a.view(), "cagent-image;")
	advanceDialog(r, r.Continue(), dialogOpenDuration+dialogResizeDuration)
	a.tick("", 40, 12)
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
