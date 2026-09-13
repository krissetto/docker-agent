package dialog

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyScrollDialogUsesIntrinsicContentHeight(t *testing.T) {
	d := newReadOnlyScrollDialog(readOnlyScrollDialogSize{
		widthPercent:  80,
		minWidth:      20,
		maxWidth:      60,
		heightPercent: 80,
		heightMax:     32,
	}, func(contentWidth, _ int) []string {
		return []string{
			RenderTitle("Title", contentWidth, lipgloss.NewStyle()),
			RenderSeparator(contentWidth),
			"",
			"one",
		}
	})
	d.SetSize(80, 40)

	view := d.View()
	require.NotEmpty(t, view)
	assert.Equal(t, fixedLines+dialogChrome+1, lipgloss.Height(view),
		"chrome must wrap the intrinsic header, one content row, and footer exactly once")
	row, _ := d.Position()
	assert.Equal(t, (40-lipgloss.Height(view))/2, row,
		"short content must be centered by its rendered intrinsic height")
}
