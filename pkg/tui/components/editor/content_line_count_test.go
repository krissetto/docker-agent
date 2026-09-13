package editor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentLineCountVisualWrappedUnicode(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "empty", value: "", want: 1},
		{name: "soft wrap ASCII", value: "hello world", want: 2},
		{name: "logical lines and wrap", value: "hello world\nfoo", want: 3},
		{name: "wide Unicode uses cell width", value: "界界界界界界", want: 2},
		{name: "combining Unicode uses grapheme width", value: "e\u0301e\u0301e\u0301e\u0301e\u0301e\u0301", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(nil).(*editor)
			e.textarea.Prompt = ""
			e.textarea.ShowLineNumbers = false
			e.textarea.SetWidth(10)
			e.textarea.SetHeight(10)
			e.SetValue(tt.value)

			assert.Equal(t, tt.want, e.ContentLineCount())
		})
	}
}

func TestContentLineCountPreservesViewportAndView(t *testing.T) {
	e := New(nil).(*editor)
	e.textarea.SetWidth(8)
	e.textarea.SetHeight(3)
	e.SetValue("abcdefghijk\nsecond long line\nthird long line\nfourth")
	_ = e.View()
	e.textarea.MoveToBegin()
	for e.textarea.Line() < e.textarea.LineCount()-1 {
		e.textarea.CursorEnd()
		e.textarea.CursorDown()
	}
	e.textarea.SetCursorColumn(3)

	beforeLine := e.textarea.Line()
	beforeColumn := e.textarea.Column()
	beforeYOffset := e.textarea.ScrollYOffset()
	beforeView := e.View()
	require.Positive(t, beforeYOffset)
	require.Less(t, beforeColumn, len([]rune("fourth")))
	require.Greater(t, e.ContentLineCount(), 4)

	assert.Equal(t, beforeLine, e.textarea.Line())
	assert.Equal(t, beforeColumn, e.textarea.Column())
	assert.Equal(t, beforeYOffset, e.textarea.ScrollYOffset())
	assert.Equal(t, beforeView, e.View())
}

func TestSetSizeRepairsViewportForAutogrowAndManualResize(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(12, 2)
	e.SetValue("first wrapped line here\nsecond wrapped line here\nlast")
	beforeLine := e.textarea.Line()
	beforeInfo := e.textarea.LineInfo()

	// Autogrow and a width-only manual resize both exercise viewport repair.
	e.SetSize(12, 6)
	e.SetSize(18, 6)
	afterInfo := e.textarea.LineInfo()

	assert.Equal(t, beforeLine, e.textarea.Line())
	assert.Equal(t, beforeInfo.StartColumn+beforeInfo.ColumnOffset,
		afterInfo.StartColumn+afterInfo.ColumnOffset)
	assert.Contains(t, e.View(), "last")
}
