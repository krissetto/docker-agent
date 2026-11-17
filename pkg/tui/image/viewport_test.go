package image

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestViewportFitUpscalesWithinCanvasWithoutChangingFit(t *testing.T) {
	p, err := DecodePreview("small", previewFixture(t, 12, 8, false))
	require.NoError(t, err)
	defer p.Close()
	fit, err := p.Fit(10, 4, CellSize{8, 16})
	require.NoError(t, err)
	assert.Equal(t, 12, fit.Width)
	lines, err := p.RenderViewport(10, 4, CellSize{8, 16}, Viewport{})
	require.NoError(t, err)
	require.Len(t, lines, 4)
	cols, marker := MarkerColumns(lines[0])
	assert.True(t, marker)
	assert.Equal(t, 10, cols)
	clean, overlays := extractOverlays(strings.Join(lines, "\n"))
	assert.Equal(t, "\n\n\n", clean, "markers contain no padding")
	require.Len(t, overlays, 1)
	assert.Equal(t, 10, overlays[0].placementCols)
	assert.Equal(t, 53, overlays[0].displayHeight)
	assert.Equal(t, 12, overlays[0].sourceW)
	assert.Equal(t, 8, overlays[0].sourceH)
}

func TestViewportZoomPanBoundsAndAspect(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {17, 35}, {1000, 10}, {10, 1000}, {1200, 800}} {
		p, err := DecodePreview("image", previewFixture(t, size[0], size[1], false))
		require.NoError(t, err)
		for _, cell := range []CellSize{{}, {9, 23}} {
			for _, zoom := range []float64{0, 1, 1.2, 2, 16, math.Inf(1), math.NaN()} {
				for _, pan := range []float64{-2, 0, 2, math.NaN()} {
					lines, err := p.RenderViewport(10, 5, cell, Viewport{zoom, pan, pan})
					require.NoError(t, err)
					require.LessOrEqual(t, len(lines), 5)
					_, overlays := extractOverlays(strings.Join(lines, "\n"))
					require.Len(t, overlays, 1)
					o := overlays[0]
					assert.LessOrEqual(t, o.cols, 10)
					assert.LessOrEqual(t, o.sourceX+o.sourceW, size[0])
					assert.LessOrEqual(t, o.sourceY+o.sourceH, size[1])
					assert.Greater(t, o.sourceW, 0)
					assert.Greater(t, o.sourceH, 0)
					if pan == -2 {
						assert.Zero(t, o.sourceX)
						assert.Zero(t, o.sourceY)
					} else if pan == 2 {
						assert.Equal(t, size[0], o.sourceX+o.sourceW)
						assert.Equal(t, size[1], o.sourceY+o.sourceH)
					}
				}
			}
		}
		p.Close()
	}
}

func TestViewportReusesSourceAcrossPanZoomAndReleasesLease(t *testing.T) {
	p, err := DecodePreview("source", previewFixture(t, 1200, 800, true))
	require.NoError(t, err)
	id := p.SourceID()
	var output bytes.Buffer
	writer := NewWriter(&output)
	inlineRegistry.Lock()
	entryCount := len(inlineRegistry.entries)
	inlineRegistry.Unlock()
	for i := range 100 {
		lines, err := p.RenderViewport(30, 10, CellSize{8, 16}, Viewport{1 + float64(i%8), float64(i%3 - 1), float64(i%5-2) / 2})
		require.NoError(t, err)
		writer.SetContent(strings.Join(lines, "\n"))
		require.Len(t, writer.overlays, 1)
		assert.Equal(t, id, writer.overlays[0].id)
		assert.Same(t, &p.source.PNGData[0], &writer.overlays[0].png[0])
		_, err = writer.Write([]byte("frame"))
		require.NoError(t, err)
	}
	assert.Equal(t, 1, strings.Count(output.String(), "a=t,t=d"))
	assert.Contains(t, output.String(), ",x=")
	inlineRegistry.Lock()
	entry := inlineRegistry.entries[id].Value.(*registryEntry)
	assert.Len(t, inlineRegistry.entries, entryCount, "pan and zoom do not create registry variants")
	assert.Equal(t, 1, entry.owners)
	assert.False(t, entry.cached)
	inlineRegistry.Unlock()
	p.Close()
	assert.False(t, previewImageRetained(id))
	writer.SetContent("")
	output.Reset()
	_, err = writer.Write([]byte("closed"))
	require.NoError(t, err)
	assert.Contains(t, output.String(), fmt.Sprintf("a=d,d=I,i=%d", id))
	assert.Empty(t, writer.managed)
	_, err = p.RenderViewport(30, 10, CellSize{}, Viewport{})
	assert.Error(t, err)
}

func TestViewportClippingAndCapability(t *testing.T) {
	p, err := DecodePreview("source", previewFixture(t, 120, 80, false))
	require.NoError(t, err)
	defer p.Close()
	lines, err := p.RenderViewport(12, 5, CellSize{}, Viewport{Zoom: 2})
	require.NoError(t, err)
	require.Greater(t, len(lines), 2)
	for _, clipped := range [][]string{lines[1:], lines[:len(lines)-1], append(append([]string{}, lines[:1]...), lines[2:]...)} {
		_, overlays := extractOverlays(strings.Join(clipped, "\n"))
		assert.Empty(t, overlays, "partial scaled crops cannot bleed across layers")
	}
	writer := NewWriter(io.Discard)
	writer.SetEnabled(false)
	writer.SetContent(strings.Join(lines, "\n"))
	assert.Len(t, writer.overlays, 1, "explicit viewport bypasses automatic display preference")
	writer.SetSupported(false)
	writer.SetContent(strings.Join(lines, "\n"))
	assert.Empty(t, writer.overlays, "unsupported terminals remain text-only")
	for _, size := range [][2]int{{0, 1}, {1, 0}, {-1, 10}} {
		lines, err := p.RenderViewport(size[0], size[1], CellSize{}, Viewport{})
		require.NoError(t, err)
		assert.Empty(t, lines)
	}
}

func TestViewportPanChangesOverlayEquality(t *testing.T) {
	p, err := DecodePreview("source", previewFixture(t, 120, 80, false))
	require.NoError(t, err)
	defer p.Close()
	writer := NewWriter(io.Discard)
	for _, pan := range []float64{-1, 1} {
		lines, err := p.RenderViewport(10, 5, CellSize{}, Viewport{Zoom: 2, PanX: pan})
		require.NoError(t, err)
		writer.SetContent(strings.Join(lines, "\n"))
		assert.True(t, writer.dirty)
		_, err = writer.Write([]byte("frame"))
		require.NoError(t, err)
		assert.False(t, writer.dirty)
	}
}

func BenchmarkViewportPanZoom(b *testing.B) {
	for _, size := range [][2]int{{1024, 1024}, {3840, 2160}} {
		b.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(b *testing.B) {
			p, err := DecodePreview("image", previewFixture(b, size[0], size[1], true))
			require.NoError(b, err)
			defer p.Close()
			writer := NewWriter(io.Discard)
			// Exclude the initial source upload: interactions must remain independent
			// of source size after this one-time transfer.
			lines, err := p.RenderViewport(60, 20, CellSize{9, 23}, Viewport{Zoom: 2})
			require.NoError(b, err)
			writer.SetContent(strings.Join(lines, "\n"))
			_, err = writer.Write([]byte("frame"))
			require.NoError(b, err)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				lines, err := p.RenderViewport(60, 20, CellSize{9, 23}, Viewport{2, float64(i%200)/100 - 1, 0})
				if err != nil {
					b.Fatal(err)
				}
				writer.SetContent(strings.Join(lines, "\n"))
				if _, err := writer.Write([]byte("frame")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestViewportRenderingDoesNotReadSourceEncoding(t *testing.T) {
	// A pinned source is opaque to viewport rendering: even undecodable payload
	// bytes are passed through unchanged; all geometry uses validated dimensions.
	p := PreviewFromInline(Inline{PNGData: []byte("opaque-source"), Width: 3840, Height: 2160})
	require.NotNil(t, p)
	defer p.Close()
	for i := range 100 {
		lines, err := p.RenderViewport(60, 20, CellSize{}, Viewport{2, float64(i)/50 - 1, 0})
		require.NoError(t, err)
		_, overlays := extractOverlays(strings.Join(lines, "\n"))
		require.Len(t, overlays, 1)
		assert.Same(t, &p.source.PNGData[0], &overlays[0].png[0])
		assert.Equal(t, "opaque-source", string(overlays[0].png))
	}
}
