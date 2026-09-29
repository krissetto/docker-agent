package editor

import (
	"image"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOccupiedTextCellsExcludeNonInputSurfaces(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	assert.Empty(t, e.OccupiedTextCells(), "placeholder is not input")
	e.SetValue("   \n ")
	assert.Empty(t, e.OccupiedTextCells(), "blank cursor and whitespace do not obscure glyphs")
	e.SetValue("abc")
	e.suggestion, e.hasSuggestion = strings.Repeat("ghost", 5), true
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 3, 1)}, e.OccupiedTextCells())
	e.Blur()
	assert.Empty(t, e.OccupiedTextCells())
	e.Focus()
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 3, 1)}, e.OccupiedTextCells())
	e.SetValue("")
	assert.Empty(t, e.OccupiedTextCells())
}

func TestOccupiedTextCellsUnicodeMultilineAndAttachmentTokens(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 4)
	e.SetValue("界 e\u0301 👩‍💻\n@paste-1\n  last")
	assert.Equal(t, []image.Rectangle{
		image.Rect(0, 0, 2, 1), image.Rect(3, 0, 4, 1), image.Rect(5, 0, 7, 1),
		image.Rect(0, 1, 8, 2), image.Rect(2, 2, 6, 3),
	}, e.OccupiedTextCells())
}

func TestOccupiedTextCellsFollowWrapViewportAndResize(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(8, 2)
	e.SetValue("abcdefghijklmno")
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 8, 1), image.Rect(0, 1, 7, 2)}, e.OccupiedTextCells())
	e.SetSize(20, 2)
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 15, 1)}, e.OccupiedTextCells())

	e.SetValue("first\nsecond\nlast")
	require.Positive(t, e.textarea.ScrollYOffset())
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 6, 1), image.Rect(0, 1, 4, 2)}, e.OccupiedTextCells())
	e.ScrollByWheel(-10)
	assert.Equal(t, []image.Rectangle{image.Rect(0, 0, 5, 1), image.Rect(0, 1, 6, 2)}, e.OccupiedTextCells())
	offset, row, col := e.textarea.ScrollYOffset(), e.textarea.Line(), e.textarea.Column()
	for range 3 {
		_ = e.OccupiedTextCells()
	}
	assert.Equal(t, offset, e.textarea.ScrollYOffset())
	assert.Equal(t, row, e.textarea.Line())
	assert.Equal(t, col, e.textarea.Column())
	assert.Equal(t, "first\nsecond\nlast", e.Value())
}

func TestOccupiedTextCellsNarrowViewport(t *testing.T) {
	for _, width := range []int{1, 2, 3, 8} {
		e := New(nil).(*editor)
		e.SetViewportSize(width, 2)
		e.SetSize(width, 2)
		e.SetValue("界e\u0301abcdefgh")
		cells := e.OccupiedTextCells()
		require.NotEmpty(t, cells)
		for _, cell := range cells {
			assert.True(t, cell.In(image.Rect(0, 0, e.textarea.Width(), e.textarea.Height())), "%v width=%d", cell, width)
		}
	}
}
