package editor

import (
	"image"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// TextOccupancy exposes visible input glyphs in textarea-local cell coordinates,
// excluding the editor frame, attachment banner, placeholder, and suggestions.
type TextOccupancy interface {
	OccupiedTextCells() []image.Rectangle
}

func (e *editor) OccupiedTextCells() []image.Rectangle {
	if !e.textarea.Focused() || e.textarea.Value() == "" {
		return nil
	}

	// Use the actual viewport so wrapping, wheel scrolling, and Unicode clipping
	// follow textarea rendering rather than a second approximation of its layout.
	var cells []image.Rectangle
	for y, line := range strings.Split(ansi.Strip(e.textarea.View()), "\n") {
		if y >= e.textarea.Height() {
			break
		}
		x := 0
		graphemes := uniseg.NewGraphemes(line)
		for graphemes.Next() {
			width := graphemes.Width()
			end := min(x+width, e.textarea.Width())
			if end > x && strings.TrimFunc(graphemes.Str(), unicode.IsSpace) != "" {
				if last := len(cells) - 1; last >= 0 && cells[last].Min.Y == y && cells[last].Max.X == x {
					cells[last].Max.X = end
				} else {
					cells = append(cells, image.Rect(x, y, end, y+1))
				}
			}
			x += width
			if x >= e.textarea.Width() {
				break
			}
		}
	}
	return cells
}
