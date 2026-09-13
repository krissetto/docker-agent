package statusbar

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// StatusBar displays the quit shortcut on the left and version info on the right.
// When the tab bar is hidden, it also shows a clickable "+ new tab" button.
type StatusBar struct {
	width int
	help  core.KeyMapHelp
	title string

	showNewTab   bool
	newTabStartX int
	newTabEndX   int

	cached     string
	cacheDirty bool
}

// Option configures a StatusBar.
type Option func(*StatusBar)

// WithTitle overrides the default "docker agent" title.
func WithTitle(title string) Option {
	return func(s *StatusBar) { s.title = title }
}

// New creates a new StatusBar instance.
func New(help core.KeyMapHelp, opts ...Option) StatusBar {
	s := StatusBar{help: help, title: "docker agent", cacheDirty: true}
	for _, opt := range opts {
		opt(&s)
	}
	return s
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

// SetShowNewTab controls whether the footer offers a new-tab button.
func (s *StatusBar) SetShowNewTab(show bool) {
	if s.showNewTab != show {
		s.showNewTab = show
		s.cacheDirty = true
	}
}

// ClickedNewTab reports whether x hits the fully visible new-tab button.
func (s *StatusBar) ClickedNewTab(x int) bool {
	if s.cacheDirty {
		s.rebuild()
	}
	return s.showNewTab && x >= s.newTabStartX && x < s.newTabEndX
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
	s.newTabStartX, s.newTabEndX = 0, 0
	width := max(s.width, 0)
	s.cached = strings.Repeat(" ", width)
	const pad = 1
	innerWidth := width - 2*pad
	if innerWidth <= 0 {
		return
	}

	var left string
	if s.help != nil {
		if help := s.help.Help(); help != nil {
			for _, binding := range help.ShortHelp() {
				hint := binding.Help()
				if !binding.Enabled() || hint.Desc != "quit" || hint.Key == "" {
					continue
				}
				label := styles.HighlightWhiteStyle.Render(hint.Key) + " " + styles.SecondaryStyle.Render(hint.Desc)
				if lipgloss.Width(label) <= innerWidth {
					left = label
				}
				break
			}
		}
	}

	leftWidth := lipgloss.Width(left)
	rightWidth := innerWidth - leftWidth
	if leftWidth > 0 {
		rightWidth = max(0, rightWidth-1)
	}
	// Keep quit intact on narrow terminals; omit a button rather than expose a partial target.
	right := ansi.Truncate(styles.MutedStyle.Render(s.title), rightWidth, "")
	if s.showNewTab {
		button := styles.MutedStyle.Render(" │ ") +
			styles.HighlightWhiteStyle.Render("+") + styles.SecondaryStyle.Render(" new tab")
		buttonWidth := lipgloss.Width(button)
		if buttonWidth+2+lipgloss.Width(right) <= rightWidth {
			right = button + "  " + right
			s.newTabStartX = width - pad - lipgloss.Width(right)
			s.newTabEndX = s.newTabStartX + buttonWidth
		}
	}

	gap := innerWidth - leftWidth - lipgloss.Width(right)
	s.cached = " " + left + strings.Repeat(" ", gap) + right + " "
}

// View renders [ quit shortcut ... (+ new tab) docker agent VERSION ].
func (s *StatusBar) View() string {
	if s.cacheDirty {
		s.rebuild()
	}
	return s.cached
}
