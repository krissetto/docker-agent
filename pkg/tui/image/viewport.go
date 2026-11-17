package image

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Viewport selects a fitted image or a zoomed crop. Pan coordinates range from
// -1 (left/top) through 0 (center) to 1 (right/bottom); excess values are clamped.
// Zoom <= 1 fits the image, including enlarging small sources.
type Viewport struct {
	Zoom       float64
	PanX, PanY float64
}

// RenderViewport renders marker-only rows bounded by a fixed cell viewport.
// Geometry alone changes during zoom/pan: the original leased PNG is uploaded
// once by Writer, and Kitty performs scaling and cropping. No variants are kept.
// Unlike Fit, this path can enlarge an image to fill the available canvas.
func (p *Preview) RenderViewport(cols, rows int, cell CellSize, view Viewport) ([]string, error) {
	if p.source.registryID == 0 {
		return nil, errors.New("image preview is closed")
	}
	if cols <= 0 || rows <= 0 {
		return nil, nil
	}
	cell = cell.Resolved()
	zoom := view.Zoom
	if math.IsNaN(zoom) || zoom < 1 {
		zoom = 1
	}
	w, h := p.source.Width, p.source.Height
	maxW, maxH := float64(cols)*float64(cell.Width), float64(rows)*float64(cell.Height)
	scale := math.Min(maxW/float64(w), maxH/float64(h)) * zoom
	cropW := max(1, int(math.Min(float64(w), maxW/scale)))
	cropH := max(1, int(math.Min(float64(h), maxH/scale)))
	x := viewportPan(w-cropW, view.PanX)
	y := viewportPan(h-cropH, view.PanY)

	// Use only one Kitty scaling axis, preserving source aspect ratio. Compare
	// the best whole-cell width and height fits, including very narrow sources.
	placementCols, placementRows := 0, 0
	displayW, displayH := 0, 0
	for c := cols; c >= 1; c-- {
		cw := c * cell.Width
		ch := scaledDimension(cw, cropH, cropW)
		if float64(ch) <= maxH {
			placementCols, displayW, displayH = c, cw, ch
			break
		}
	}
	for r := rows; r >= 1; r-- {
		rh := r * cell.Height
		rw := scaledDimension(rh, cropW, cropH)
		if float64(rw) <= maxW {
			if rw*rh > displayW*displayH {
				placementCols, placementRows, displayW, displayH = 0, r, rw, rh
			}
			break
		}
	}
	if displayW == 0 || displayH == 0 {
		return nil, errors.New("image cannot fit within one terminal cell")
	}
	imageCols := (displayW + cell.Width - 1) / cell.Width
	imageRows := (displayH + cell.Height - 1) / cell.Height
	lines := make([]string, imageRows)
	for row := range lines {
		lines[row] = fmt.Sprintf("%s%d;%d;%d;%d;viewport;%d;%d;%d;%d;%d;%d;%d;%d;%d;%d\x1b\\", markerPrefix,
			p.source.registryID, imageCols, imageRows, row, cell.Width, cell.Height,
			placementCols, placementRows, displayW, displayH, x, y, cropW, cropH)
	}
	return lines, nil
}

func viewportPan(overflow int, pan float64) int {
	if math.IsNaN(pan) {
		pan = 0
	}
	return int(math.Round(float64(overflow) * (max(-1, min(1, pan)) + 1) / 2))
}

// appendViewportOverlay accepts only complete, bounded source rectangles. Like
// scaled previews, partially occluded viewports are suppressed rather than
// risking a rounded placement painting across another layer.
func appendViewportOverlay(overlays []overlay, fields []string, x, y int) []overlay {
	id, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return overlays
	}
	img, ok := registeredImage(uint32(id))
	if !ok {
		return overlays
	}
	var values [15]int
	for i := 1; i < len(fields); i++ {
		if i == 4 {
			continue
		}
		values[i], err = strconv.Atoi(fields[i])
		if err != nil || values[i] < 0 {
			return overlays
		}
	}
	cols, rows, row := values[1], values[2], values[3]
	cell := CellSize{values[5], values[6]}
	pc, pr, dw, dh := values[7], values[8], values[9], values[10]
	sx, sy, sw, sh := values[11], values[12], values[13], values[14]
	if !cell.Valid() || cols <= 0 || rows <= 0 || row >= rows || (pc == 0) == (pr == 0) || dw <= 0 || dh <= 0 ||
		sw <= 0 || sh <= 0 || sw > img.Width || sh > img.Height || sx > img.Width-sw || sy > img.Height-sh ||
		(dw-1)/cell.Width+1 != cols || (dh-1)/cell.Height+1 != rows {
		return overlays
	}
	next := overlay{
		explicit: true, cellHeight: cell.Height, placementCols: pc, placementRows: pr, displayHeight: dh,
		firstRow: row, totalRows: rows, id: uint32(id), png: img.PNGData, managed: img.registryID != 0,
		x: x, y: y, cols: cols, rows: 1, pixelW: img.Width, pixelH: img.Height,
		sourceX: sx, sourceY: sy, sourceW: sw, sourceH: sh,
	}
	if len(overlays) > 0 {
		last := &overlays[len(overlays)-1]
		previous := *last
		previous.y, previous.rows, previous.firstRow = y, 1, row
		if last.y+last.rows == y && last.firstRow+last.rows == row && sameOverlays([]overlay{previous}, []overlay{next}) {
			last.rows++
			return overlays
		}
	}
	return append(overlays, next)
}
