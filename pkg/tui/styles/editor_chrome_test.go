package styles

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditorChromeReferenceGeometryAndBackground(t *testing.T) { //nolint:paralleltest // style globals
	old := EditorStyle
	oldGhost := SuggestionGhostStyle
	t.Cleanup(func() {
		EditorStyle = old
		SuggestionGhostStyle = oldGhost
	})

	EditorStyle = lipgloss.NewStyle().
		Margin(0, EditorHMargin).
		Padding(1, AppPadding, 1, AppPadding).
		Background(lipgloss.Color("#123456"))
	SuggestionGhostStyle = lipgloss.NewStyle().Background(lipgloss.Color("#123456"))

	assert.Equal(t, 2, EditorHMargin)
	assert.Equal(t, 2, EditorStyle.GetMarginLeft())
	assert.Equal(t, 2, EditorStyle.GetMarginRight())
	assert.Equal(t, 1, EditorStyle.GetPaddingTop())
	assert.Equal(t, 1, EditorStyle.GetPaddingBottom())
	assert.Equal(t, EditorStyle.GetBackground(), SuggestionGhostStyle.GetBackground())

	view := RenderComposite(EditorStyle, "x\n ")
	lines := strings.Split(view, "\n")
	require.Len(t, lines, 4, "top and bottom padding each occupy one row")
	for i, line := range lines {
		assert.Equal(t, 7, lipgloss.Width(line), "row %d includes exact margins and padding", i)
		assert.Containsf(t, line, "48;2;18;52;86", "row %d carries editor background", i)
	}
}
