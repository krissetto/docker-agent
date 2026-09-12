package tabbar

import (
	"image/color"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// defaultMaxTitleLen is the default maximum display length for a tab title
	// when no user-configured value is provided.
	defaultMaxTitleLen = 20
	// defaultTabTitle is used when a tab has no title set.
	defaultTabTitle = "New Session"
	// closeButtonText is the close button content rendered inside each tab.
	closeButtonText = " ×"
	// accentBar is the leading vertical bar that indicates focus state.
	accentBar = "▎"
	// runningIndicator is shown before the title when the tab's session is streaming.
	// Cycles through braille spinner frames when animated.
	// attachedIndicator gives viewer tabs a stable identity independent of title.
	attachedIndicator = "↳ "
	// pendingIndicator is static: work has been admitted but has not begun.
	pendingIndicator = "· "
	// descendantIndicator identifies activity below this tab's own node.
	descendantIndicator = "◇ "
	// descendantAttentionIndicator is a single-cell combined state: nested work
	// remains visible while warning color/bold still communicates attention.
	descendantAttentionIndicator = "◆ "
	// attentionIndicator is shown before the title when the tab needs attention,
	// replacing the running indicator to signal that user action is required.
	attentionIndicator = "! "

	// dragSourceColorBoost controls how much the drag source tab is blended toward
	// the active tab colors when it is not the active tab.
	dragSourceColorBoost = 0.4
	// dragBystanderDimAmount controls how much non-dragged tabs are faded toward
	// their background during drag-and-drop.
	dragBystanderDimAmount = 0.65
)

// Tab represents a single rendered tab in the tab bar.
type Tab struct {
	view        string
	mainZoneEnd int // X offset where close zone begins (relative to tab start)
	width       int
}

// View returns the rendered tab content.
func (t Tab) View() string { return t.view }

// Width returns the total rendered width of the tab.
func (t Tab) Width() int { return t.width }

// MainZoneEnd returns the X offset where the main click area ends
// and the close-button click area begins.
func (t Tab) MainZoneEnd() int { return t.mainZoneEnd }

// dragRole describes a tab's role during a drag-and-drop operation.
type dragRole int

const (
	dragRoleNone      dragRole = iota // no drag in progress
	dragRoleSource                    // this tab is being dragged
	dragRoleBystander                 // another tab is being dragged; dim this one
)

// blendColors mixes two colors by the given ratio (0 = a, 1 = b).
func blendColors(a, b color.Color, ratio float64) color.Color {
	ar, ag, ab := styles.ColorToRGB(a)
	br, bg, bb := styles.ColorToRGB(b)
	return styles.RGBToColor(
		ar+(br-ar)*ratio,
		ag+(bg-ag)*ratio,
		ab+(bb-ab)*ratio,
	)
}

// renderTab creates a Tab: ▎ title ×
// The accent bar is bright for the focused tab and dim for inactive tabs.
// The close button color is computed dynamically for optimal contrast against
// the tab's background, so it works in any theme without manual tuning.
// Indicator colors (running, attention) preserve their semantic hue but are
// automatically boosted when contrast against the tab background is too low.
//
// When role is dragRoleBystander, all foreground colors are faded toward the
// background to visually de-emphasize tabs that aren't being dragged.
func renderTab(info messages.TabInfo, maxTitleLen int, role dragRole, elapsed time.Duration) Tab {
	title := info.Title
	if title == "" {
		title = defaultTabTitle
	}
	if r := []rune(title); len(r) > maxTitleLen {
		title = string(r[:maxTitleLen-1]) + "…"
	}

	// Pick colors based on focus state.
	var bgColor, fgColor, barColor color.Color
	if info.IsActive {
		bgColor = styles.TabActiveBg
		fgColor = styles.TabActiveFg
		barColor = styles.TabBorder // bright accent for focused tab
	} else {
		bgColor = styles.TabBg
		fgColor = styles.TabInactiveFg
		barColor = styles.TabInactiveFg // dim accent for inactive tab
	}

	// Lift an inactive drag source partway toward active colors so it stands
	// out from dimmed bystanders without looking fully focused.
	if role == dragRoleSource && !info.IsActive {
		fgColor = blendColors(fgColor, styles.TabActiveFg, dragSourceColorBoost)
		barColor = blendColors(barColor, styles.TabBorder, dragSourceColorBoost)
	}

	// Close button color derived from this tab's background.
	closeFg := styles.MutedContrastFg(bgColor)

	// Fade all foreground elements when this tab is a bystander during drag.
	if role == dragRoleBystander {
		fgColor = blendColors(fgColor, bgColor, dragBystanderDimAmount)
		barColor = blendColors(barColor, bgColor, dragBystanderDimAmount)
		closeFg = blendColors(closeFg, bgColor, dragBystanderDimAmount)
	}

	// Helper: every segment gets the same background.
	pad := lipgloss.NewStyle().Background(bgColor)

	// Leading accent bar.
	bar := lipgloss.NewStyle().Foreground(barColor).Background(bgColor).Render(accentBar)

	titleSt := lipgloss.NewStyle().Foreground(fgColor).Background(bgColor)
	if info.IsActive {
		titleSt = titleSt.Bold(true)
	}

	content := bar
	switch {
	case info.NeedsAttention:
		// Attention normally wins. Descendant work uses one combined glyph rather
		// than stacked indicators, preserving both semantics in the same cell.
		attnFg := styles.EnsureContrast(styles.Warning, bgColor)
		if role == dragRoleBystander {
			attnFg = blendColors(attnFg, bgColor, dragBystanderDimAmount)
		}
		indicator := attentionIndicator
		if info.Activity == messages.TabActivityDescendantRunning {
			indicator = descendantAttentionIndicator
		}
		content += lipgloss.NewStyle().Foreground(attnFg).Background(bgColor).Bold(true).Render(indicator)
	case info.Activity != messages.TabActivityNone || info.IsRunning:
		runFg := styles.EnsureContrast(styles.TabAccentFg, bgColor)
		if role == dragRoleBystander {
			runFg = blendColors(runFg, bgColor, dragBystanderDimAmount)
		}
		indicator := pendingIndicator
		switch info.Activity {
		case messages.TabActivityRunning:
			indicator = animation.Card.FrameAt(elapsed) + " "
		case messages.TabActivityDescendantRunning:
			indicator = descendantIndicator
		case messages.TabActivityNone:
			// Backward-compatible callers setting only IsRunning.
			indicator = animation.Card.FrameAt(elapsed) + " "
		}
		content += lipgloss.NewStyle().Foreground(runFg).Background(bgColor).Render(indicator)
	default:
		content += pad.Render(" ")
	}
	if info.IsAttached {
		markerFg := styles.MutedContrastFg(bgColor)
		if role == dragRoleBystander {
			markerFg = blendColors(markerFg, bgColor, dragBystanderDimAmount)
		}
		content += lipgloss.NewStyle().Foreground(markerFg).Background(bgColor).Render(attachedIndicator)
	}
	content += titleSt.Render(title)

	mainEnd := lipgloss.Width(content)

	closeBtn := lipgloss.NewStyle().Foreground(closeFg).Background(bgColor).Render(closeButtonText)
	content += closeBtn + pad.Render(" ")

	width := lipgloss.Width(content)

	return Tab{
		view:        content,
		mainZoneEnd: mainEnd,
		width:       width,
	}
}
