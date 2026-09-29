package editor

import (
	"image"
	"slices"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TextOccupancy exposes visible input glyphs in textarea-local cell coordinates,
// excluding the editor frame, attachment banner, placeholder, and suggestions.
type TextOccupancy interface {
	OccupiedTextCells() []image.Rectangle
}

// textOccupancyKey uses textarea state rather than editor messages: input can
// also change via completions, history, paste, and direct host calls. Cursor
// motion can change the rendered viewport without changing the text. Blinking
// and suggestions only change styling/non-input surfaces, not occupied glyphs.
type textOccupancyKey struct {
	value                 string
	width, height, offset int
	row, column           int
	themeGeneration       uint64
}

type textOccupancyCache struct {
	key   textOccupancyKey
	cells []image.Rectangle
	valid bool
}

func (e *editor) OccupiedTextCells() []image.Rectangle {
	if !e.textarea.Focused() {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	value := e.textarea.Value()
	if value == "" {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	if e.themeGeneration != styles.ThemeGeneration() {
		e.refreshTheme()
	}
	key := textOccupancyKey{
		value: value,
		width: e.textarea.Width(), height: e.textarea.Height(),
		offset: e.textarea.ScrollYOffset(),
		row:    e.textarea.Line(), column: e.textarea.Column(),
		themeGeneration: e.themeGeneration,
	}
	if e.occupancy.valid && e.occupancy.key == key {
		return slices.Clone(e.occupancy.cells)
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
	e.occupancy = textOccupancyCache{key: key, cells: cells, valid: true}
	// Keep callers' slices independent of the cache and later editor updates.
	return slices.Clone(cells)
}
