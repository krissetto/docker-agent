// Package contextbar provides a thin progress indicator showing how full the
// LLM context window is. It renders a single row of half-block characters (▄)
// whose colour transitions from Info → Warning → Error as usage increases.
package contextbar

import (
	"fmt"
	"image/color"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// Height is the rendered height of the context bar (one row of block chars).
	Height = 1

	// blockFill is U+2584 LOWER HALF BLOCK — fills the bottom half of each
	// terminal cell, producing a thin stripe at the foot of the row.
	blockFill = "▄"

	// warningThreshold is the usage fraction above which the fill colour
	// starts blending from Info toward Warning.
	warningThreshold = 0.45
	// criticalThreshold is the usage fraction above which the fill colour
	// starts blending from Warning toward Error.
	criticalThreshold = 0.65
)

const (
	animDuration = 670 * time.Millisecond
)

// Model holds the state for the context-usage bar.
type Model struct {
	width          int
	contextPercent float64

	// Animation state
	anim      animation.Transition
	animFrom  float64
	animTo    float64
	animating bool
}

// New creates a context bar owned by runtime.
func New(runtime *animation.Runtime) *Model {
	if runtime == nil {
		panic("contextbar: nil animation runtime")
	}
	return &Model{anim: animation.NewTransition(runtime)}
}

// Cancel stops any in-flight animation.
func (m *Model) Cancel() {
	if m.animating {
		m.anim.Cancel()
		m.animating = false
	}
}

// SetWidth sets the available rendering width.
func (m *Model) SetWidth(width int) { m.width = width }

func usagePercent(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return math.Max(0, math.Min(1, float64(used)/float64(limit)))
}

// SetContextUsage updates context-window utilisation with an animated transition.
// A zero limit disables the fill indicator.
// Returns a command to start the animation tick chain when the context changes.
// The caller MUST execute the returned command; dropping it leaks an animation
// registration and can freeze all subsequent spinners.
func (m *Model) SetContextUsage(used, limit int64) tea.Cmd {
	newPercent := usagePercent(used, limit)
	target := m.contextPercent
	if m.animating {
		target = m.animTo
	}

	// Target changes always retarget from the currently displayed value. A
	// repeated event for the in-flight target must not restart the transition.
	if math.Abs(newPercent-target) > 0.001 {
		if m.animating {
			m.anim.Cancel()
		}
		m.animFrom = m.contextPercent
		m.animTo = newPercent
		m.animating = true
		return m.anim.Start(animDuration, animation.EaseOutCubic)
	}

	return nil
}

// SetContextUsageDirect initializes context-window utilisation without
// animation. Runtime target changes should use SetContextUsage so they retarget
// continuously from the currently displayed value.
func (m *Model) SetContextUsageDirect(used, limit int64) {
	newPercent := usagePercent(used, limit)

	// If we're already animating toward this target, let the animation finish
	// naturally. This prevents View()-time calls from killing in-flight
	// transitions started by SetContextUsage.
	if m.animating && math.Abs(newPercent-m.animTo) < 0.001 {
		return
	}

	// Cancel any in-flight animation so we don't leave a stale registration.
	if m.animating {
		m.anim.Cancel()
		m.animating = false
	}

	m.contextPercent = newPercent
	m.animFrom = newPercent
	m.animTo = newPercent
}

// Update handles animation tick messages.
func (m *Model) Update(msg tea.Msg) {
	if tick, ok := msg.(animation.TickMsg); ok && m.animating {
		before := m.View()
		m.anim.Tick()
		v := m.anim.Value()
		m.contextPercent = m.animFrom + (m.animTo-m.animFrom)*v
		if !m.anim.Running() {
			m.contextPercent = m.animTo
			m.animating = false
		}
		if before != m.View() {
			tick.MarkDirty()
		}
	}
}

// View renders the bar as a single line of [blockFill] characters followed
// by a "Context xx%" label. Each cell uses TabBg as its background (so the
// top half matches the editor) and the foreground color determines the bottom
// half. The bar fills left to right: the solid portion uses a dim tint of the
// fill color, then a short gradient ramps up to the full fill color so the
// rightmost filled cell (the tip) is the brightest. Track cells use the
// terminal background, creating a seamless transition from the input area to
// the terminal below.
func (m *Model) View() string {
	if m.width <= 0 {
		return ""
	}

	pct := int(math.Round(m.contextPercent * 100))
	label := fmt.Sprintf(" Context %d%%", pct)
	labelWidth := lipgloss.Width(label)
	if labelWidth > m.width {
		label = ""
		labelWidth = 0
	}

	barWidth := m.width - labelWidth

	fillWidth := 0
	if m.contextPercent > 0 && barWidth > 0 {
		// Any non-zero usage occupies the leading cell. Rounding very small
		// percentages to zero makes the bar look empty and loses the only cell
		// that distinguishes positive usage from the track.
		fillWidth = max(1, int(math.Round(float64(barWidth)*m.contextPercent)))
		fillWidth = min(fillWidth, barWidth)
	}

	const gradientLen = 5
	gradWidth := min(gradientLen, fillWidth)
	solidWidth := fillWidth - gradWidth
	trackWidth := barWidth - fillWidth

	fillFg := fillColor(m.contextPercent)
	trackFg := styles.Background
	baseFg := blendColor(trackFg, fillFg, 1.0/float64(gradientLen+1))
	bg := lipgloss.NewStyle().Background(styles.TabBg)

	var sb strings.Builder
	if solidWidth > 0 {
		sb.WriteString(bg.Foreground(baseFg).Render(
			strings.Repeat(blockFill, solidWidth)))
	}
	for i := range gradWidth {
		t := float64(i+1) / float64(gradWidth+1)
		c := blendColor(trackFg, fillFg, t)
		sb.WriteString(bg.Foreground(c).Render(blockFill))
	}
	if trackWidth > 0 {
		sb.WriteString(bg.Foreground(trackFg).Render(
			strings.Repeat(blockFill, trackWidth)))
	}

	if label != "" {
		labelStyle := lipgloss.NewStyle().Foreground(styles.TextMuted)
		sb.WriteString(labelStyle.Render(label))
	}

	return sb.String()
}

// --- colour helpers ----------------------------------------------------------

func colorToLinear(c color.Color) (float64, float64, float64) {
	r, g, b, _ := c.RGBA()
	return float64(r) / 65535, float64(g) / 65535, float64(b) / 65535
}

func blendColor(a, b color.Color, ratio float64) color.Color {
	ar, ag, ab := colorToLinear(a)
	br, bg, bb := colorToLinear(b)

	clamp := func(v float64) int {
		switch {
		case v < 0:
			return 0
		case v > 1:
			return 255
		default:
			return int(math.Round(v * 255))
		}
	}

	ri := clamp(ar + (br-ar)*ratio)
	gi := clamp(ag + (bg-ag)*ratio)
	bi := clamp(ab + (bb-ab)*ratio)

	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", ri, gi, bi))
}

// fillColor returns the foreground colour for the filled [blockFill] characters.
// The hue transitions Info → Warning → Error with smooth blending.
func fillColor(pct float64) color.Color {
	switch {
	case pct >= criticalThreshold:
		criticalRange := 1.0 - criticalThreshold
		t := (pct - criticalThreshold) / criticalRange
		return blendColor(styles.Warning, styles.Error, t)

	case pct >= warningThreshold:
		warningRange := criticalThreshold - warningThreshold
		t := (pct - warningThreshold) / warningRange
		return blendColor(styles.Info, styles.Warning, t)

	default:
		return styles.Info
	}
}
