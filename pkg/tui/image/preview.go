package image

import (
	"bytes"
	"fmt"
	stdimage "image"
	"image/png"
	"math"

	"golang.org/x/image/draw"
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

// Preview retains native decoded pixels; fitting never changes automatic chat image limits.
type Preview struct {
	source stdimage.Image
	fitted Inline
}

func DecodePreview(name string, data []byte) (*Preview, error) {
	img, _, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return &Preview{source: img, fitted: Inline{Name: name, MIME: "image/png"}}, nil
}

// Fit downsamples only when fitted pixel dimensions change; callers own update-time preparation.
func (p *Preview) Fit(cols, rows int, cell CellSize) (Inline, error) {
	cell = cell.Resolved()
	bounds := p.source.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	scale := min(1.0, float64(max(1, cols)*cell.Width)/float64(w), float64(max(1, rows)*cell.Height)/float64(h))
	w, h = max(1, int(math.Floor(float64(w)*scale))), max(1, int(math.Floor(float64(h)*scale)))
	if p.fitted.Width == w && p.fitted.Height == h {
		return p.fitted, nil
	}
	img := p.source
	if w != bounds.Dx() || h != bounds.Dy() {
		resized := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(resized, resized.Bounds(), img, bounds, draw.Over, nil)
		img = resized
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return Inline{}, err
	}
	p.fitted.Width, p.fitted.Height, p.fitted.PNGData = w, h, encoded.Bytes()
	return p.fitted, nil
}

// RenderNativePreviewMarkers reserves cell extents but places fitted PNG pixels at native size.
func RenderNativePreviewMarkers(img Inline, cell CellSize) []string {
	cell = cell.Resolved()
	cols, rows := (img.Width+cell.Width-1)/cell.Width, (img.Height+cell.Height-1)/cell.Height
	if cols <= 0 || rows <= 0 || len(img.PNGData) == 0 {
		return nil
	}
	id := registerImage(imageID(img.PNGData), img)
	lines := make([]string, rows)
	for row := range lines {
		lines[row] = fmt.Sprintf("%s%d;%d;%d;%d;native;%d;%d\x1b\\", markerPrefix, id, cols, rows, row, cell.Width, cell.Height)
	}
	return lines
}
