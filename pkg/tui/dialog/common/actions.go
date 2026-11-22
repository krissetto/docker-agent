package common

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Action describes a footer pill and the existing shortcut it dispatches.
// Escape is never shown as a badge, even on the Cancel pill.
type Action struct {
	Label string
	Key   rune
	Badge string
}

// ActionBar keeps keyboard focus and records the exact terminal cells occupied
// by each pill. Dialogs render it last, above the frame's bottom padding.
type ActionBar struct {
	selected int
	actions  []Action
	hits     []actionHit
	height   int
}

type actionHit struct {
	x, y, width int
}

// Select sets the highlighted pill before entering the footer from another zone.
func (b *ActionBar) Select(index int) { b.selected = index }

// Selected reports the current pill index.
func (b *ActionBar) Selected() int { return b.selected }

// Key translates Enter into the focused shortcut. Tab goes in either
// direction without changing what the dialog's explicit shortcuts do.
func (b *ActionBar) Key(msg tea.KeyPressMsg) (tea.KeyPressMsg, bool) {
	if len(b.actions) == 0 {
		return msg, false
	}
	switch msg.Keystroke() {
	case "tab":
		b.selected = (b.selected + 1) % len(b.actions)
		return tea.KeyPressMsg{}, true
	case "shift+tab":
		b.selected = (b.selected + len(b.actions) - 1) % len(b.actions)
		return tea.KeyPressMsg{}, true
	case "enter":
		return actionKey(b.actions[b.selected].Key), false
	}
	return msg, false
}

func actionKey(code rune) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Code: code}
	if code >= 32 && code <= 126 {
		k.Text = string(code)
	}
	return k
}

// FooterAction returns the shortcut of the pill at the given screen cell.
func (b *ActionBar) FooterAction(x, y, row, col, height int, frame lipgloss.Style) (tea.KeyPressMsg, bool) {
	x -= col + frame.GetBorderLeftSize() + frame.GetPaddingLeft()
	y -= row + height - frame.GetBorderBottomSize() - frame.GetPaddingBottom() - b.height
	for i, hit := range b.hits {
		if y == hit.y && x >= hit.x && x < hit.x+hit.width {
			b.selected = i
			return actionKey(b.actions[i].Key), true
		}
	}
	return tea.KeyPressMsg{}, false
}

// PillStyle uses the same selection colors as list selections. Inactive pills
// remain visibly actionable without competing with the focused one.
func PillStyle(selected bool) lipgloss.Style {
	style := styles.NoStyle.Bold(true).Foreground(styles.TextPrimary).Background(styles.BackgroundAlt)
	if selected {
		style = style.Foreground(styles.SelectedFg).Background(styles.Selected)
	}
	return style
}

// Render lays out complete pills in terminal cells. The space reserved for
// the Enter marker keeps geometry stable as focus moves.
func (b *ActionBar) Render(width int, alignment lipgloss.Position, actions ...Action) string {
	b.actions = actions
	if b.selected >= len(actions) {
		b.selected = 0
	}
	b.hits = b.hits[:0]
	padding := min(1, max(0, (width-1)/2))
	var lines []string
	var line strings.Builder
	lineWidth, first := 0, 0
	flush := func() {
		offset := int(float64(max(0, width-lineWidth)) * float64(alignment))
		for i := first; i < len(b.hits); i++ {
			b.hits[i].x += offset
		}
		lines = append(lines, strings.Repeat(" ", offset)+line.String())
		first = len(b.hits)
		line.Reset()
		lineWidth = 0
	}
	for i, action := range actions {
		selected := i == b.selected
		label := action.Label
		if action.Badge != "" {
			label += " " + action.Badge
		}
		limit := max(1, width-2*padding)
		if limit >= 4 {
			label = ansi.Truncate(label, limit-2, "…")
			if selected {
				label += " ↵"
			} else {
				label += "  "
			}
		}
		pill := PillStyle(selected).Padding(0, padding).Render(ansi.Truncate(label, limit, "…"))
		cells := lipgloss.Width(pill)
		if lineWidth > 0 && lineWidth+1+cells > width {
			flush()
		}
		if lineWidth > 0 {
			line.WriteByte(' ')
			lineWidth++
		}
		b.hits = append(b.hits, actionHit{x: lineWidth, y: len(lines), width: cells})
		line.WriteString(pill)
		lineWidth += cells
	}
	if lineWidth > 0 {
		flush()
	}
	b.height = len(lines)
	return strings.Join(lines, "\n")
}

// ApplyActionKey handles footer traversal and dispatch before the dialog's
// regular key map. Rendering prepares the action list even on first input.
func (b *BaseDialog) ApplyActionKey(msg tea.KeyPressMsg, view func() string) (tea.KeyPressMsg, bool) {
	if len(b.Actions.actions) == 0 {
		view()
	}
	return b.Actions.Key(msg)
}

// ApplyActionClick translates a left click on a pill to its direct shortcut.
func (b *BaseDialog) ApplyActionClick(msg tea.MouseClickMsg, view string, row, col int, frame lipgloss.Style) (tea.KeyPressMsg, bool) {
	if msg.Button != tea.MouseLeft {
		return tea.KeyPressMsg{}, false
	}
	return b.Actions.FooterAction(msg.X, msg.Y, row, col, lipgloss.Height(view), frame)
}
