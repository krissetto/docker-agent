// Package viewport provides a fixed-size, non-wrapping text viewport.
// It keeps ANSI styling intact when clipping or scrolling horizontally; callers
// own any surrounding chrome and scrollbar.
package viewport

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	wheelStep      = 3
	horizontalStep = 6
)

// Model holds text and its scroll position. Content is immutable between calls
// to SetContent, so Update can safely return a value copy.
type Model struct {
	width, height    int
	xOffset, yOffset int
	longestLine      int
	lines            []string
}

// New returns an empty viewport. Set its dimensions before rendering.
func New() Model { return Model{} }

// SetWidth sets the visible column count.
func (m *Model) SetWidth(width int) {
	m.width = max(0, width)
	m.clampOffsets()
}

// SetHeight sets the visible row count.
func (m *Model) SetHeight(height int) {
	m.height = max(0, height)
	m.clampOffsets()
}

// Height returns the visible row count.
func (m Model) Height() int { return m.height }

// TotalLineCount returns the number of content lines, without soft wrapping.
func (m Model) TotalLineCount() int { return len(m.lines) }

// YOffset returns the index of the first visible content line.
func (m Model) YOffset() int { return m.yOffset }

// SetContent replaces the text and normalizes CRLF line endings.
func (m *Model) SetContent(content string) {
	m.lines = strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	m.longestLine = 0
	for _, line := range m.lines {
		m.longestLine = max(m.longestLine, ansi.StringWidth(line))
	}
	if len(m.lines) == 1 && m.longestLine == 0 {
		m.lines = nil
	}
	m.clampOffsets()
}

// GetContent returns the full text, with normalized line endings.
func (m Model) GetContent() string { return strings.Join(m.lines, "\n") }

// GotoTop scrolls to the first line, retaining the horizontal position.
func (m *Model) GotoTop() { m.yOffset = 0 }

// GotoBottom scrolls to the last page.
func (m *Model) GotoBottom() { m.yOffset = max(0, len(m.lines)-m.height) }

// ScrollDown moves by n rows (negative values scroll upward).
func (m *Model) ScrollDown(n int) {
	m.yOffset += n
	m.clampOffsets()
}

func (m *Model) clampOffsets() {
	m.yOffset = min(max(0, m.yOffset), max(0, len(m.lines)-m.height))
	m.xOffset = min(max(0, m.xOffset), max(0, m.longestLine-m.width))
}

// Update handles pager keys and wheel scrolling. Shift-wheel and horizontal
// wheel events scroll columns instead of rows.
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	var rows, cols int
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "down", "j":
			rows = 1
		case "up", "k":
			rows = -1
		case "pgdown", "space", "f":
			rows = m.height
		case "pgup", "b":
			rows = -m.height
		case "d", "ctrl+d":
			rows = m.height / 2
		case "u", "ctrl+u":
			rows = -m.height / 2
		case "right", "l":
			cols = horizontalStep
		case "left", "h":
			cols = -horizontalStep
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			if msg.Mod.Contains(tea.ModShift) {
				cols = horizontalStep
			} else {
				rows = wheelStep
			}
		case tea.MouseWheelUp:
			if msg.Mod.Contains(tea.ModShift) {
				cols = -horizontalStep
			} else {
				rows = -wheelStep
			}
		case tea.MouseWheelRight:
			cols = horizontalStep
		case tea.MouseWheelLeft:
			cols = -horizontalStep
		}
	}
	m.yOffset += rows
	m.xOffset += cols
	m.clampOffsets()
	return m, nil
}

// View clips content to the visible columns and pads every row to exactly the
// viewport dimensions. Long lines never wrap or change the dialog geometry.
func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	visible := make([]string, m.height)
	for row := range visible {
		line := ""
		if index := m.yOffset + row; index < len(m.lines) {
			line = ansi.Cut(m.lines[index], m.xOffset, m.xOffset+m.width)
		}
		visible[row] = line + strings.Repeat(" ", max(0, m.width-ansi.StringWidth(line)))
	}
	return strings.Join(visible, "\n")
}
