package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitPaneAreasTinyHeightsNeverOverflow(t *testing.T) {
	t.Parallel()

	root := &appModel{paneGeometry: splitGeometry{Panes: map[string]splitRect{"a": {}, "b": {}}}}
	for h := 0; h <= 5; h++ {
		areas := root.paneAreas(splitRect{X: 1, Y: 1, W: 10, H: h})
		assert.Equal(t, h, areas.transcript.H+areas.gap.H+areas.title.H, "height %d", h)
		assert.GreaterOrEqual(t, areas.transcript.H, 0)
		assert.LessOrEqual(t, areas.title.H, 1)
	}
}
