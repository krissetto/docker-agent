package todotool

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func renderTodoIcon(status string) (string, lipgloss.Style) {
	switch status {
	case "pending":
		return "○", styles.ToBeDoneStyle
	case "in-progress":
		return "◐", styles.InProgressStyle
	case "completed":
		return "●", styles.CompletedStyle
	default:
		return "?", styles.ToBeDoneStyle
	}
}

// StatusLabel deliberately includes text as well as color and a symbol.
func StatusLabel(status string) string {
	icon, style := renderTodoIcon(status)
	return style.Render(icon + " " + status)
}

func NextStatus(status string) string {
	switch status {
	case "pending":
		return "in-progress"
	case "in-progress":
		return "completed"
	default:
		return "pending"
	}
}

// StatusIcon is the compact, independently clickable status control.
func StatusIcon(status string) string {
	icon, style := renderTodoIcon(status)
	return style.Render(icon)
}

// Description styles each wrapped line independently so continuation rows agree.
func Description(text, status string, selected bool) string {
	_, style := renderTodoIcon(status)
	if selected {
		style = style.Bold(true)
	}
	if status == "completed" {
		// Lipgloss strikethrough decorates individual runes, splitting ZWJ graphemes.
		return "\x1b[9m" + style.Render(text) + "\x1b[29m"
	}
	return style.Render(text)
}

// RowLines wraps only the description; controls are never repeated or hard-wrapped.
func RowLines(description, status string, width int, selected bool) []string {
	width = max(1, width)
	indent := 6
	if width < 8 {
		indent = 0
	}
	wrapped := strings.Split(ansi.Hardwrap(ansi.Wordwrap(ansi.Strip(strings.ReplaceAll(description, "\r", "")), max(1, width-indent), ""), max(1, width-indent), true), "\n")
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}
	rows := make([]string, len(wrapped))
	prefix := StatusIcon(status) + " " + styles.MutedStyle.Render("× ✎ ")
	for i, line := range wrapped {
		lead := strings.Repeat(" ", indent)
		if i == 0 {
			lead = ansi.Truncate(prefix, indent, "")
		}
		rows[i] = lead + Description(ansi.Truncate(line, max(1, width-indent), ""), status, selected)
	}
	return rows
}

// RowActions is the cell contract shared by compact sidebar paint and pointer hits.
// Negative action columns mean the complete cluster cannot fit beside the text.
type RowActions struct {
	Width, TextWidth     int
	Status, Edit, Remove int
}

func RightActions(width int, withStatus bool) RowActions {
	width = max(1, width)
	a := RowActions{Width: width, TextWidth: width, Status: -1, Edit: -1, Remove: -1}
	reserve := 4
	if withStatus {
		reserve = 6
	}
	if width < reserve+2 {
		return a
	}
	a.TextWidth = width - reserve
	a.Edit, a.Remove = width-3, width-1
	if withStatus {
		a.Status = width - 5
	}
	return a
}

func (a RowActions) PartAt(col int, first bool) string {
	if first && col >= 0 {
		switch col {
		case a.Status:
			return "status"
		case a.Edit:
			return "edit"
		case a.Remove:
			return "remove"
		}
	}
	return "text"
}

func (a RowActions) Render(text, status string, first bool) string {
	text = ansi.Truncate(text, a.TextWidth, "")
	text += strings.Repeat(" ", max(0, a.TextWidth-ansi.StringWidth(text)))
	if !first || a.Remove < 0 {
		return text
	}
	if a.Status >= 0 {
		text += " " + StatusIcon(status)
	}
	return text + " " + styles.MutedStyle.Render("✎ ×")
}

// SidebarActions overlays edit/remove on the first line without reserving text width.
func SidebarActions(width int) RowActions {
	width = max(1, width)
	prefix := min(2, width-1)
	a := RowActions{Width: width, TextWidth: width - prefix, Status: -1, Edit: -1, Remove: -1}
	if prefix > 0 {
		a.Status = 0
	}
	if width >= 8 {
		a.Edit, a.Remove = width-3, width-1
	}
	return a
}

func sidebarRowLines(description, status string, width int) []string {
	a := SidebarActions(width)
	prefix := a.Width - a.TextWidth
	wrapped := strings.Split(ansi.Hardwrap(ansi.Wordwrap(ansi.Strip(strings.ReplaceAll(description, "\r", "")), a.TextWidth, ""), a.TextWidth, true), "\n")
	rows := make([]string, len(wrapped))
	for i, line := range wrapped {
		lead := strings.Repeat(" ", prefix)
		if i == 0 && a.Status >= 0 {
			lead = StatusIcon(status) + strings.Repeat(" ", prefix-1)
		}
		rows[i] = lead + Description(ansi.Truncate(line, a.TextWidth, ""), status, false)
	}
	return rows
}
