package sidebar

import (
	"strings"

	"charm.land/lipgloss/v2"
)

func (m *model) footerHeight() int {
	if m.sessionState.YoloMode() && m.height > 0 {
		return 1
	}
	return 0
}

func (m *model) viewportHeight() int { return max(0, m.height-m.footerHeight()) }

func (m *model) footerView(width int) string {
	pill := m.yoloIndicator(width)
	if pill == "" {
		return ""
	}
	return strings.Repeat(" ", max(0, width-lipgloss.Width(pill))) + pill
}
