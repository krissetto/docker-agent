package editor

import (
	"image"
	"slices"
	"strings"
	"unicode"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TextOccupancy exposes visible input glyphs in textarea-local cell coordinates,
// excluding the editor frame, attachment banner, placeholder, and suggestions.
type TextOccupancy interface {
	OccupiedTextCells() []image.Rectangle
}

// TextOccupancyAppender appends screen-space cells to caller-owned storage.
// Unlike borrowing the editor's cache, the returned cells can safely be retained
// or modified by the caller across subsequent editor updates.
type TextOccupancyAppender interface {
	AppendOccupiedTextCells(dst []image.Rectangle, origin image.Point) []image.Rectangle
}

// textOccupancyKey uses textarea state rather than editor messages: input can
// also change via completions, history, paste, and direct host calls. Cursor
// motion can change the rendered viewport without changing the text. Blinking
// and suggestions only change styling/non-input surfaces, not occupied glyphs.
type textOccupancyKey struct {
	contentRevision       uint64
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
	return slices.Clone(e.occupiedTextCells())
}

func (e *editor) AppendOccupiedTextCells(dst []image.Rectangle, origin image.Point) []image.Rectangle {
	cells := e.occupiedTextCells()
	dst = slices.Grow(dst, len(cells))
	for _, cell := range cells {
		dst = append(dst, cell.Add(origin))
	}
	return dst
}

func (e *editor) occupiedTextCells() []image.Rectangle {
	if !e.textarea.Focused() {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	if e.textarea.Value() == "" {
		e.occupancy = textOccupancyCache{}
		return nil
	}
	if e.themeGeneration != styles.ThemeGeneration() {
		e.refreshTheme()
	}
	key := textOccupancyKey{
		contentRevision: e.textarea.ContentRevision(),
		width:           e.textarea.Width(), height: e.textarea.Height(),
		offset: e.textarea.ScrollYOffset(),
		row:    e.textarea.Line(), column: e.textarea.Column(),
		themeGeneration: e.themeGeneration,
	}
	if e.occupancy.valid && e.occupancy.key == key {
		return e.occupancy.cells
	}

	// Source runs exclude presentation-only prompts, placeholder, padding and ghosts.
	geometry := e.textarea.Layout()
	var cells []image.Rectangle
	for index, row := range geometry.Rows {
		y := index - geometry.ScrollY
		if y < 0 {
			continue
		}
		if y >= geometry.Height {
			break
		}
		for _, run := range row.Runs {
			x, end := max(0, run.Cell-geometry.ScrollX), min(geometry.Width, run.Cell+run.Width-geometry.ScrollX)
			if end <= x || strings.TrimFunc(run.Text, unicode.IsSpace) == "" {
				continue
			}
			if last := len(cells) - 1; last >= 0 && cells[last].Min.Y == y && cells[last].Max.X == x {
				cells[last].Max.X = end
			} else {
				cells = append(cells, image.Rect(x, y, end, y+1))
			}
		}
	}
	e.occupancy = textOccupancyCache{key: key, cells: cells, valid: true}
	return cells
}
