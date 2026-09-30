package image

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtPositionUsesOnlyVisibleGraphicCellsAndPinsCachedSource(t *testing.T) {
	img := Inline{Name: "cached.png", PNGData: previewFixture(t, 40, 80, false), Width: 40, Height: 80}
	lines := RenderMarkers(img, 24)
	for i := range lines {
		lines[i] += "                      "
	}
	view := "caption\n" + strings.Join(lines, "\n") + "\nfooter"
	_, ok := AtPosition(view, 2, 0)
	assert.False(t, ok)
	_, ok = AtPosition(view, 1, 1)
	assert.False(t, ok)
	hit, ok := AtPosition(view, 2, 1)
	require.True(t, ok)
	assert.Equal(t, img.PNGData, hit.PNGData)
	_, ok = AtPosition(view, 22, 1)
	assert.False(t, ok, "right padding is not part of image")
	_, ok = AtPosition(view, 2, len(lines)+1)
	assert.False(t, ok)
	clipped := strings.Join(lines[3:5], "\n")
	hit, ok = AtPosition(clipped, 5, 0)
	require.True(t, ok, "vertically scrolled visible rows remain clickable")
	p := PreviewFromInline(hit)
	require.NotNil(t, p)
	id := p.SourceID()
	assert.NotZero(t, id)
	for i := range maxInlineRegistryEntries + 1 {
		registerImage(uint32(100+i), Inline{PNGData: []byte{byte(i), 99}})
	}
	assert.True(t, previewImageRetained(id), "pressed image survives automatic-cache eviction until release")
	p.Close()
	assert.False(t, previewImageRetained(id))
	_, ok = AtPosition(StripMarkers(clipped), 5, 0)
	assert.False(t, ok)
}
