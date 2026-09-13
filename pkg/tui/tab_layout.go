package tui

import "github.com/docker/docker-agent/pkg/tui/styles"

// Tabs align with the editor frame, not the inset editable text.
func tabFrameOrigin() int {
	return styles.EditorStyle.GetMarginLeft()
}

func tabFrameWidth(width int) int {
	return max(0, width-styles.EditorStyle.GetHorizontalMargins())
}
