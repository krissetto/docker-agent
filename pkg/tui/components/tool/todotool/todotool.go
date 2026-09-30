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
