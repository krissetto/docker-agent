package tabbar

import (
	"image/color"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	buttonHoverDuration = 150 * time.Millisecond
	buttonPulseDuration = 280 * time.Millisecond
)

type tabButton struct {
	sessionID string
	plus      bool
}

type buttonHover struct {
	motion   animation.Transition
	from, to float64
}

func (h *buttonHover) fraction() float64 {
	return h.from + (h.to-h.from)*h.motion.Value()
}

func (t *TabBar) hoverFraction(button tabButton) float64 {
	if hover := t.buttonHover[button]; hover != nil {
		return hover.fraction()
	}
	return 0
}

func (t *TabBar) plusStyle() lipgloss.Style {
	pulse := 0.0
	if t.plusPulse.Running() {
		pulse = 1 - t.plusPulse.Value()
	}
	return buttonStyle(styles.Background, styles.MutedContrastFg(styles.Background), t.hoverFraction(tabButton{plus: true}), pulse)
}

func buttonStyle(bg, fg color.Color, hover, pulse float64) lipgloss.Style {
	// These are the composer's semantic palette equivalents of the
	// reference tab-hover and tab-press surfaces.
	if hover > 0 {
		bg = blendColors(bg, styles.BackgroundAlt, 0.3*hover)
		fg = blendColors(fg, styles.TextBright, 0.3*hover)
	}
	if pulse > 0 {
		bg = blendColors(bg, styles.Selected, 0.7*pulse)
		fg = blendColors(fg, styles.TextBright, 0.7*pulse)
	}
	return lipgloss.NewStyle().Background(bg).Foreground(fg)
}

// UpdateHover handles tab-local pointer cells; a negative x leaves the strip.
func (t *TabBar) UpdateHover(x int) tea.Cmd {
	var target tabButton
	if !t.drag.active {
		for _, z := range t.zones {
			if x >= z.startX && x < z.endX {
				switch {
				case z.isPlus:
					target.plus = true
				case z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
					target.sessionID = t.tabs[z.tabIdx].SessionID
				}
				break
			}
		}
	}
	if target == t.hoveredButton {
		return nil
	}
	var leave, enter tea.Cmd
	if previous := t.buttonHover[t.hoveredButton]; previous != nil {
		previous.from, previous.to = previous.fraction(), 0
		leave = previous.motion.Start(buttonHoverDuration, animation.EaseOutCubic)
	}
	if target != (tabButton{}) {
		if t.buttonHover == nil {
			t.buttonHover = make(map[tabButton]*buttonHover)
		}
		next := t.buttonHover[target]
		if next == nil {
			next = &buttonHover{motion: t.ar.Transition()}
			t.buttonHover[target] = next
		}
		next.from, next.to = next.fraction(), 1
		enter = next.motion.Start(buttonHoverDuration, animation.EaseOutCubic)
	}
	t.hoveredButton = target
	return tea.Batch(leave, enter)
}

// TickHover advances button fades on the owner's clock.
func (t *TabBar) TickHover(tick animation.TickMsg) {
	for button, hover := range t.buttonHover {
		if hover.motion.Running() {
			hover.motion.Tick()
			tick.MarkDirty()
		}
		if !hover.motion.Running() && hover.to == 0 {
			delete(t.buttonHover, button)
		}
	}
	if t.plusPulse.Running() {
		t.plusPulse.Tick()
		tick.MarkDirty()
	}
}

// CancelHover immediately resets feedback when the strip loses pointer ownership.
func (t *TabBar) CancelHover() {
	if len(t.buttonHover) > 0 || t.plusPulse.Running() {
		t.visualGeneration++
	}
	for _, hover := range t.buttonHover {
		hover.motion.Cancel()
	}
	clear(t.buttonHover)
	t.hoveredButton = tabButton{}
	t.plusPulse.Cancel()
}
