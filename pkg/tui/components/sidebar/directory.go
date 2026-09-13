package sidebar

import (
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	directoryIcon     = "↗"
	directoryCopyIcon = "⎘"
)

func directoryIconWidth() int { return ansi.StringWidth(directoryIcon) }
func directoryReserve() int   { return ansi.StringWidth(directoryCopyIcon) + directoryIconWidth() + 3 }

func (m *model) directoryRow(width int) string {
	if m.workingDirLine() == "" {
		return ""
	}
	reserve := directoryReserve()
	if width < reserve+2 {
		return padRight(m.hoverText(ansi.Truncate(m.workingDirLine(), max(0, width-1), "…"), "directory"), width)
	}
	name := ansi.Truncate(m.workingDirLine(), width-reserve, "…")
	progress := m.hoverValues["directory"].value
	name = styles.HoverText(name, progress, styles.TextPrimary)
	copyIcon := styles.FadeLine(styles.HoverText(styles.MutedStyle.Render(directoryCopyIcon), m.hoverValues["directory-copy"].value, styles.TextPrimary), progress)
	icon := styles.FadeLine(styles.HoverText(styles.MutedStyle.Render(directoryIcon), m.hoverValues["directory-open"].value, styles.TextPrimary), progress)
	return padRight(name, width-reserve) + " " + copyIcon + " " + icon + " "
}

func (m *model) directoryIconHit(x, width int) bool {
	return width >= directoryReserve()+2 && x >= width-directoryIconWidth()-1 && x < width-1 && (m.hoverTarget == "directory" || m.hoverTarget == "directory-open" || m.hoverValues["directory"].value > 0 || m.hoverValues["directory-open"].value > 0)
}
