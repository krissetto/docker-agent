package image

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// AtPosition resolves only graphics occupying the already-rendered viewport.
// Captions and padding are not image targets; clipped/absent markers cannot hit.
func AtPosition(view string, x, y int) (Inline, bool) {
	if x < 0 || y < 0 {
		return Inline{}, false
	}
	lines := strings.Split(view, "\n")
	if y >= len(lines) {
		return Inline{}, false
	}
	width := ansi.StringWidth(lines[y])
	for _, placed := range extractMarkerOverlays(lines) {
		if placed.x < 0 || placed.x+placed.cols > width || x < placed.x || x >= placed.x+placed.cols || y < placed.y || y >= placed.y+placed.rows {
			continue
		}
		img, ok := registeredImage(placed.id)
		return img, ok
	}
	return Inline{}, false
}

// PreviewFromInline pins the immutable rendered source without I/O or decoding.
// Callers transfer ownership to a dialog or Close it when a gesture is cancelled.
func PreviewFromInline(img Inline) *Preview {
	if len(img.PNGData) == 0 || img.Width <= 0 || img.Height <= 0 {
		return nil
	}
	img.placementCols, img.placementRows = 0, 0
	img.registryID = acquirePreviewImage(img)
	return &Preview{source: img}
}

func (p *Preview) Name() string     { return p.source.Name }
func (p *Preview) SourceID() uint32 { return p.source.registryID }
