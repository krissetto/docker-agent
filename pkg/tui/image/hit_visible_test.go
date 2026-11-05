package image

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtPositionUsesOnlyVisibleGraphicCells(t *testing.T) {
	previous := renderingDisabled.Load()
	SetRenderingEnabled(true)
	t.Cleanup(func() { renderingDisabled.Store(previous) })

	img := Inline{Name: "cached.png", PNGData: []byte("hit-test-png"), Width: 40, Height: 80}
	lines := RenderMarkers(img, 24)
	require.GreaterOrEqual(t, len(lines), 5)
	for i := range lines {
		lines[i] += "                      "
	}
	view := "caption\n" + strings.Join(lines, "\n") + "\nfooter"

	_, ok := AtPosition(view, 2, 0)
	assert.False(t, ok, "captions are not image targets")
	_, ok = AtPosition(view, 1, 1)
	assert.False(t, ok, "left of the image is not a target")
	hit, ok := AtPosition(view, 2, 1)
	require.True(t, ok)
	assert.Equal(t, img.PNGData, hit.PNGData)
	_, ok = AtPosition(view, 22, 1)
	assert.False(t, ok, "right padding is not part of the image")
	_, ok = AtPosition(view, 2, len(lines)+1)
	assert.False(t, ok, "footer row has no image")

	clipped := strings.Join(lines[3:5], "\n")
	hit, ok = AtPosition(clipped, 5, 0)
	require.True(t, ok, "vertically scrolled visible rows remain clickable")
	assert.Equal(t, img.PNGData, hit.PNGData)
	_, ok = AtPosition(StripMarkers(clipped), 5, 0)
	assert.False(t, ok, "stripped markers cannot hit")
}
