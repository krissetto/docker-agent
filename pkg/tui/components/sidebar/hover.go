package sidebar

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const hoverDuration = 150 * time.Millisecond

type hoverValue struct {
	value, target, from float64
	elapsed             time.Duration
}

func (m *model) hoverText(text, key string) string {
	return styles.HoverText(text, m.hoverValues[key].value, styles.TextPrimary)
}

func (m *model) setHoverTarget(key string) tea.Cmd {
	if !m.presentationActive {
		return nil
	}
	if key == m.hoverTarget {
		return nil
	}
	if strings.HasPrefix(m.hoverTarget, "queue:") || strings.HasPrefix(m.hoverTarget, "queue-remove:") || strings.HasPrefix(key, "queue:") || strings.HasPrefix(key, "queue-remove:") {
		delete(m.sectionCache, "queue")
	}
	m.hoverTarget = key
	m.invalidateHover()
	m.syncBranchHover()
	if m.hoverValues == nil {
		m.hoverValues = make(map[string]hoverValue)
	}
	active := map[string]bool{}
	if key == "directory" || key == "directory-open" {
		active["directory"] = true
		if key == "directory-open" {
			active["directory-open"] = true
		} else {
			active["directory-copy"] = true
		}
	} else if key != "" {
		active[key] = true
	}
	for name := range active {
		if _, ok := m.hoverValues[name]; !ok {
			m.hoverValues[name] = hoverValue{}
		}
	}
	for name, state := range m.hoverValues {
		target := 0.0
		if active[name] {
			target = 1
		}
		if state.target != target {
			state.from, state.target, state.elapsed = state.value, target, 0
			m.hoverValues[name] = state
		}
	}
	for _, state := range m.hoverValues {
		if state.value != state.target {
			return m.hoverAnimation.Start()
		}
	}
	return nil
}

func (m *model) tickHover(tick animation.TickMsg) {
	if !m.hoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	step := float64(after-before) / float64(hoverDuration)
	changed, running := false, false
	for key, state := range m.hoverValues {
		old := state.value
		switch {
		case strings.HasPrefix(key, "directory"):
			state.elapsed += after - before
			p := min(1, float64(state.elapsed)/float64(animation.ShortDuration))
			state.value = state.from + (state.target-state.from)*animation.EaseOutCubic(p)
		case state.target > state.value:
			state.value = min(state.target, state.value+step)
		default:
			state.value = max(state.target, state.value-step)
		}
		changed = changed || old != state.value
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(m.hoverValues, key)
		} else {
			m.hoverValues[key] = state
		}
	}
	if !running {
		m.hoverAnimation.Stop()
	}
	if changed {
		delete(m.sectionCache, "queue")
		m.invalidateHover()
		tick.MarkDirty()
	}
}

func (m *model) cancelHover() {
	m.hoverAnimation.Stop()
	m.hoverValues = nil
	m.hoverTarget = ""
	m.hoveredRegion = ClickNone
	m.hoveredSubagent = ""
	m.hoveredParent = false
	m.hoveredTreeRow = -1
	m.branchCapture = branchCapture{}
}

// StopAnimation releases this sidebar's runtime leases when its page is removed.
func (m *model) StopAnimation() {
	m.CancelPresentation()
	m.spinner.Stop()
	m.subagentSpinner.Stop()
	m.transferAnimation.Stop()
	for _, state := range m.ragIndexing {
		state.spinner.Stop()
	}
	m.spinnerActive = false
	m.subagentSpinnerOn = false
}

// CancelHover clears pointer presentation when this page stops receiving ticks.
func (m *model) CancelHover() {
	changed := len(m.hoverValues) > 0 || m.hoverTarget != "" || m.hoveredRegion != ClickNone || m.hoveredSubagent != "" || m.hoveredParent || m.hoveredTreeRow != -1
	m.cancelHover()
	if changed {
		m.invalidateHover()
	}
}

// Pointer styling leaves semantic rows and section geometry unchanged. Do not
// mask an already pending spinner/data refresh with this narrower invalidation.
func (m *model) invalidateHover() {
	onlyHover := !m.cacheDirty || m.hoverOnlyDirty
	m.invalidateAnimation()
	m.hoverOnlyDirty = onlyHover
}
