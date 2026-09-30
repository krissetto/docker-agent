package tui

import (
	"bytes"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

func TestAttachmentImageClickUsesWriterAvailabilityWithoutChangingDraft(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 40, 40))))
	for _, mode := range []string{"enabled", "disabled", "unsupported", "nil"} {
		t.Run(mode, func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			defer root.ar.Stop()
			defer root.dialogMgr.Cleanup()
			var out bytes.Buffer
			if mode != "nil" {
				root.imageWriter = tuiimage.NewWriter(&out)
				root.imageWriter.SetEnabled(mode != "disabled")
				root.imageWriter.SetSupported(mode != "unsupported")
			}
			if mode == "disabled" {
				tuiimage.SetRenderingEnabled(false)
				t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })
			}
			path := filepath.Join(t.TempDir(), "preview.png")
			require.NoError(t, os.WriteFile(path, data.Bytes(), 0o600))
			require.NoError(t, root.editor.AttachFile(path))
			draft := root.editor.Value()
			root.editor.ToggleContextBar()
			root.resizeAll()
			root.View()
			bannerBefore := root.editor.BannerView(root.width)
			focusBefore := root.focusedPanel
			assert.False(t, root.editor.IsContextBarFocused())
			_, _ = root.Update(tea.MouseClickMsg{X: 2, Y: root.composerLayout().bannerTop + 2, Button: tea.MouseLeft})
			require.True(t, root.dialogMgr.Open())
			view := root.dialogMgr.TopDialog().View()
			if mode == "enabled" || mode == "disabled" {
				assert.NotContains(t, ansi.Strip(view), "pixels")
			} else {
				assert.Contains(t, ansi.Strip(view), "40 × 40 pixels")
			}
			if mode == "enabled" || mode == "disabled" {
				assert.Contains(t, view, "cagent-image;")
			} else {
				assert.NotContains(t, view, "cagent-image;")
				assert.Contains(t, ansi.Strip(view), "does not support image rendering")
			}
			assert.Equal(t, draft, root.editor.Value())
			assert.False(t, root.editor.IsContextBarFocused(), "attachment preview never focuses/highlights the context rectangle")
			assert.Equal(t, focusBefore, root.focusedPanel)
			assert.Equal(t, bannerBefore, root.editor.BannerView(root.width))
			if mode == "enabled" || mode == "disabled" {
				root.imageWriter.SetContent(view)
				_, err := root.imageWriter.Write([]byte("preview"))
				require.NoError(t, err)
				assert.Contains(t, out.String(), "a=p,i=", "explicit preview renders even when automatic images are disabled")
			}
			if root.imageWriter != nil {
				assert.Equal(t, mode == "enabled", root.imageWriter.RenderingEnabled(), "preview never changes the writer preference")
			}
			if mode == "disabled" {
				assert.Empty(t, tuiimage.RenderMarkers(tuiimage.Inline{PNGData: []byte("automatic"), Width: 10, Height: 10}, 20))
			}
		})
	}
}

func TestImageDialogComposedFrameOwnsPlacementsThroughStackAndResize(t *testing.T) {
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 40, 80))))
	d := dialog.NewImageAttachmentPreviewDialog(nil, "image.png", "image/png", data.Bytes(), true)
	var output bytes.Buffer
	writer := tuiimage.NewWriter(&output)
	frame := func(overlay bool, width, height int) {
		d.SetSize(width, height)
		row, col := d.Position()
		layers := []*lipgloss.Layer{lipgloss.NewLayer(d.View()).X(col).Y(row)}
		if overlay {
			layers = append(layers, lipgloss.NewLayer(strings.Repeat(" ", width)).Y(height/2).Z(1))
		}
		content := composeRootLayers(layers, width, height)
		clean := writer.SetContent(content)
		assert.NotContains(t, clean, "cagent-image;")
		_, err := writer.Write([]byte("frame"))
		require.NoError(t, err)
	}
	frame(false, 80, 24)
	require.Contains(t, output.String(), "a=p,i=", "real dialog markers survive complete composition")
	output.Reset()
	frame(true, 80, 24)
	assert.Contains(t, output.String(), "a=d,d=a", "occlusion replaces stale placement geometry")
	output.Reset()
	frame(false, 10, 7)
	assert.Contains(t, output.String(), "a=d,d=a", "resize replaces stale placement geometry")
	output.Reset()
	writer.SetContent("replacement dialog without graphics")
	_, err := writer.Write([]byte("closed"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), "a=d,d=a")
	assert.NotContains(t, output.String(), "a=p,i=", "close/replacement leaves no graphics placement")
}

func TestImageCellMetricsReflowPreviewAndRejectInvalidReports(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	defer root.dialogMgr.Cleanup()
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 800, 600))))
	preview := dialog.NewImageAttachmentPreviewDialog(nil, "native.png", "image/png", data.Bytes(), true)
	root.dialogMgr.Update(dialog.OpenDialogMsg{Model: preview})
	before := preview.View()
	root.Update(uv.CellSizeEvent{Width: 9, Height: 23})
	require.Equal(t, tuiimage.CellSize{Width: 9, Height: 23}, root.imageCellSize)
	assert.NotEqual(t, before, preview.View(), "valid cell-size report refits active image outside View")
	warm := preview.View()
	root.Update(uv.CellSizeEvent{Width: 0, Height: 9999})
	assert.Equal(t, warm, preview.View())
	for range 4 {
		assert.Equal(t, warm, preview.View())
	}
}
