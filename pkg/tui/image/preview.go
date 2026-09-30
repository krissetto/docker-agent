package image

import (
	"bytes"
	"errors"
	"fmt"
	stdimage "image"
	"image/png"
)

// CellSize is terminal cell geometry in pixels. Unknown terminals use 8×16.
type CellSize struct{ Width, Height int }

func (s CellSize) Valid() bool {
	return s.Width > 0 && s.Height > 0 && s.Width <= 512 && s.Height <= 512
}
func (s CellSize) Resolved() CellSize {
	if !s.Valid() {
		return CellSize{8, 16}
	}
	return s
}

// Preview owns one validated source upload, not a history of resized variants.
type Preview struct{ source Inline }

func DecodePreview(name string, data []byte) (*Preview, error) {
	img, format, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var encoded []byte
	if format == "png" {
		encoded = bytes.Clone(data)
	} else {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		encoded = bytes.Clone(buf.Bytes())
	}
	bounds := img.Bounds()
	source := Inline{Name: name, MIME: "image/png", PNGData: encoded, Width: bounds.Dx(), Height: bounds.Dy()}
	source.registryID = acquirePreviewImage(source)
	shared, _ := registeredImage(source.registryID)
	source.PNGData = shared.PNGData
	return &Preview{source: source}, nil
}

// Fit selects one-axis terminal scaling: Kitty computes the other dimension
// from the source aspect ratio. Whole-cell quantization never enlarges an image.
// https://sw.kovidgoyal.net/kitty/graphics-protocol/#displaying-images
func (p *Preview) Fit(cols, rows int, cell CellSize) (Inline, error) {
	if p.source.registryID == 0 {
		return Inline{}, errors.New("image preview is closed")
	}
	cell = cell.Resolved()
	maxW, maxH := max(1, cols)*cell.Width, max(1, rows)*cell.Height
	fit := p.source
	if fit.Width <= maxW && fit.Height <= maxH {
		return fit, nil
	}
	bestW, bestH := 0, 0
	for c := min(cols, p.source.Width/cell.Width); c >= 1; c-- {
		w := c * cell.Width
		h := scaledDimension(w, p.source.Height, p.source.Width)
		if h <= maxH {
			fit.placementCols = c
			bestW, bestH = w, h
			break
		}
	}
	for r := min(rows, p.source.Height/cell.Height); r >= 1; r-- {
		h := r * cell.Height
		w := scaledDimension(h, p.source.Width, p.source.Height)
		if w <= maxW {
			if w*h > bestW*bestH {
				fit.placementCols = 0
				fit.placementRows = r
				bestW, bestH = w, h
			}
			break
		}
	}
	if bestW <= 0 || bestH <= 0 {
		return Inline{}, errors.New("image cannot fit within one terminal cell")
	}
	fit.Width, fit.Height = bestW, bestH
	return fit, nil
}

func scaledDimension(value, numerator, denominator int) int {
	return max(1, (value*numerator+denominator/2)/denominator)
}

// RenderNativePreviewMarkers records actual display extents while retaining the
// original data ID. The writer scales placements, never the uploaded PNG data.
func RenderNativePreviewMarkers(img Inline, cell CellSize) []string {
	cell = cell.Resolved()
	cols, rows := (img.Width+cell.Width-1)/cell.Width, (img.Height+cell.Height-1)/cell.Height
	if cols <= 0 || rows <= 0 || len(img.PNGData) == 0 {
		return nil
	}
	id := img.registryID
	if id == 0 {
		id = registerImage(imageSignature(img), img)
	}
	lines := make([]string, rows)
	for row := range lines {
		if img.placementCols > 0 || img.placementRows > 0 {
			lines[row] = fmt.Sprintf("%s%d;%d;%d;%d;scaled;%d;%d;%d;%d;%d;%d\x1b\\", markerPrefix, id, cols, rows, row, cell.Width, cell.Height, img.placementCols, img.placementRows, img.Width, img.Height)
		} else {
			lines[row] = fmt.Sprintf("%s%d;%d;%d;%d;native;%d;%d\x1b\\", markerPrefix, id, cols, rows, row, cell.Width, cell.Height)
		}
	}
	return lines
}

// Close releases this preview only; other live previews and automatic images survive.
func (p *Preview) Close() { releasePreviewImage(p.source.registryID); p.source = Inline{} }
