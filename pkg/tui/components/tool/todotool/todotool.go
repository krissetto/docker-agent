package todotool

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func renderTodoIcon(status string) (string, lipgloss.Style) {
	switch status {
	case "pending":
		return "◯", styles.ToBeDoneStyle
	case "in-progress":
		return "◔", styles.InProgressStyle
	case "completed":
		return "✓", styles.CompletedStyle
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
