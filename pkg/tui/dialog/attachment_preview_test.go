package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentPreviewUsesSharedViewportAndPreservesWhitespace(t *testing.T) {
	d := NewAttachmentPreviewDialog(newDialogRuntime(), "file.txt", "    indented\n"+strings.Repeat("line\n", 40)).(*attachmentPreviewDialog)
	d.SetSize(80, 24)
	assert.Contains(t, ansi.Strip(d.View()), "    indented")
	require.True(t, d.scrollview.NeedsScrollbar())
	updated, _ := d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	assert.Same(t, d, updated)
	assert.Positive(t, d.scrollview.ScrollOffset())
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	assert.IsType(t, CloseDialogMsg{}, cmd())
	CleanupDialog(d)
}
