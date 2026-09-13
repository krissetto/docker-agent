package statusbar

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// StatusBar displays the command-palette shortcut.
type StatusBar struct {
	width int
	help  core.KeyMapHelp

	commandsStartX int
	commandsEndX   int

	cached     string
	cacheDirty bool
}

// New creates a new StatusBar instance.
func New(help core.KeyMapHelp) StatusBar {
	return StatusBar{help: help, cacheDirty: true}
}

// SetWidth sets the width of the status bar.
func (s *StatusBar) SetWidth(width int) {
	if s.width != width {
		s.width = width
		s.cacheDirty = true
	}
}

// SetHelp sets the help provider for the status bar.
func (s *StatusBar) SetHelp(help core.KeyMapHelp) {
	s.help = help
	s.cacheDirty = true
}

// ClickedCommands reports whether x hits the visible command shortcut.
func (s *StatusBar) ClickedCommands(x int) bool {
	if s.cacheDirty {
		s.rebuild()
	}
	return x >= s.commandsStartX && x < s.commandsEndX
}

// Height returns the rendered height of the status bar (always 1).
func (s *StatusBar) Height() int {
	return 1
}

// InvalidateCache clears all cached values.
func (s *StatusBar) InvalidateCache() {
	s.cacheDirty = true
}

func (s *StatusBar) rebuild() {
	s.cacheDirty = false
	s.commandsStartX, s.commandsEndX = 0, 0
	width := max(s.width, 0)
	s.cached = strings.Repeat(" ", width)
	if s.help == nil {
		return
	}
	help := s.help.Help()
	if help == nil {
		return
	}
	for _, binding := range help.ShortHelp() {
		hint := binding.Help()
		if !binding.Enabled() || hint.Desc != "commands" || hint.Key == "" {
			continue
		}
		label := styles.HighlightWhiteStyle.Render(hint.Key)
		labelWidth := lipgloss.Width(label)
		if labelWidth > width-2*styles.EditorHMargin {
			return
		}
		s.commandsStartX = styles.EditorHMargin
		s.commandsEndX = s.commandsStartX + labelWidth
		s.cached = strings.Repeat(" ", s.commandsStartX) + label + strings.Repeat(" ", width-s.commandsEndX)
		return
	}
}

// View renders the command shortcut with editor-aligned margins.
func (s *StatusBar) View() string {
	if s.cacheDirty {
		s.rebuild()
	}
	return s.cached
}
