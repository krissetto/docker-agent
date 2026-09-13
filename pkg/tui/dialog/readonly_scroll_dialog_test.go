package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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
	assert.Equal(t, 3+dialogChrome+1, lipgloss.Height(view),
		"chrome wraps the intrinsic header and one content row without a redundant Close footer")
	row, _ := d.Position()
	assert.Equal(t, (40-lipgloss.Height(view))/2, row,
		"short content must be centered by its rendered intrinsic height")
}

func TestReadOnlyDialogHasNoCloseActionOrFooterRows(t *testing.T) {
	d := NewHelpDialog(nil).(*helpDialog)
	d.SetSize(60, 20)
	view := d.View()
	require.Empty(t, d.actionRows)
	require.Zero(t, d.actionFooterHeight)
	require.Contains(t, view, dialogCloseGlyph)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	require.IsType(t, CloseDialogMsg{}, cmd())
}
