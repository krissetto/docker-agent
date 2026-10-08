package sidebar

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type hoverValue struct {
	value, target float64
}

func (m *model) hoverText(text, key string) string {
	return styles.HoverText(text, m.hoverValues[key].value, styles.TextPrimary)
}

func (m *model) setHoverTarget(key string) tea.Cmd {
	return m.setHoverTargets(key, "")
}

func (m *model) setHoverTargets(key, nameKey string) tea.Cmd {
	if !m.presentationActive {
		return nil
	}
	if key == m.hoverTarget && nameKey == m.hoverNameTarget {
		return nil
	}
	m.hoverTarget, m.hoverNameTarget = key, nameKey
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
		if strings.HasPrefix(key, "todo:") || strings.HasPrefix(key, "queue:") {
			base := key[:strings.LastIndex(key, ":")]
			active[base+":row"] = true
		}
	}
	if nameKey != "" {
		active[nameKey] = true
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
			state.target = target
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
	changed, running := false, false
	for key, state := range m.hoverValues {
		old := state.value
		state.value = animation.HoverStep(state.value, state.target, after-before)
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
		m.invalidateHover()
		tick.MarkDirty()
	}
}

func (m *model) cancelHover() {
	m.ResetTodoClick()
	m.hoverAnimation.Stop()
	m.hoverValues = nil
	m.hoverTarget, m.hoverNameTarget = "", ""
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

// A terminal's transparent background is unknown: hide glyphs, never RGB-camouflage them.
func hoverAction(text string, progress float64) string {
	if progress <= 0 {
		return strings.Repeat(" ", ansi.StringWidth(text))
	}
	return styles.HoverText(text, progress, styles.TextPrimary)
}
