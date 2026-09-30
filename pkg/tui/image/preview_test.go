package image

import (
	"bytes"
	stdimage "image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativePreviewFitPreservesNativeSizeAndFitsBothAxes(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {17, 35}, {1000, 100}, {100, 1000}, {3000, 2000}} {
		var data bytes.Buffer
		require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, size[0], size[1]))))
		preview, err := DecodePreview("image", data.Bytes())
		require.NoError(t, err)
		for _, cell := range []CellSize{{8, 16}, {9, 23}} {
			for _, box := range [][2]int{{100, 40}, {10, 5}, {200, 100}} {
				fitted, err := preview.Fit(box[0], box[1], cell)
				require.NoError(t, err)
				assert.LessOrEqual(t, fitted.Width, min(size[0], box[0]*cell.Width))
				assert.LessOrEqual(t, fitted.Height, min(size[1], box[1]*cell.Height))
				assert.InDelta(t, float64(size[0])/float64(size[1]), float64(fitted.Width)/float64(fitted.Height), float64(size[0])/float64(size[1])/float64(fitted.Height)+1/float64(fitted.Height))
				again, err := preview.Fit(box[0], box[1], cell)
				require.NoError(t, err)
				assert.Same(t, &fitted.PNGData[0], &again.PNGData[0], "identical fit reuses prepared PNG")
			}
		}
	}
}
