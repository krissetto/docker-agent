package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/help"
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
	require.Zero(t, d.bodyTitleGap)
	require.Equal(t, 1, d.bodyHeaderGap)
	require.Equal(t, 1, d.bodyHeight)
	require.Equal(t, 1, d.bodyFooterGap)
	assert.Equal(t, 2+d.bodyTitleGap+d.bodyHeaderGap+dialogChrome+1+d.bodyFooterGap+1, lipgloss.Height(view),
		"chrome wraps title, separator, shared gaps and one content row without a redundant Close footer")
	row, _ := d.Position()
	assert.Equal(t, (40-lipgloss.Height(view))/2, row,
		"short content must be centered by its rendered intrinsic height")
}

func TestReadOnlyDialogHasNoCloseActionOrFooterRows(t *testing.T) {
	d := NewHelpDialog(help.Document{}).(*helpDialog)
	d.SetSize(60, 20)
	view := d.View()
	require.Empty(t, d.actionRows)
	require.Zero(t, d.actionFooterHeight)
	require.Contains(t, view, dialogCloseGlyph)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	require.IsType(t, CloseDialogMsg{}, cmd())
}

func TestReadOnlyDialogEnforcesTotalHeightCapAndScrollReach(t *testing.T) {
	d := NewAttachmentPreviewDialog(nil, "text.txt", strings.Repeat("row\n", 100)+"LAST").(*attachmentPreviewDialog)
	for _, size := range [][2]int{{120, 100}, {80, 24}, {30, 12}, {12, 6}} {
		d.SetSize(size[0], size[1])
		assert.LessOrEqual(t, lipgloss.Height(d.View()), min(size[1]*80/100, 60))
		d.scrollview.ScrollToBottom()
		if size[0] >= 30 {
			assert.Contains(t, d.View(), "LAST")
		}
	}
}
