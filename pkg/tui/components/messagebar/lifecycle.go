package messagebar

import (
	"image/color"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Severity uint8

const (
	Info Severity = iota
	Warning
	Error
	Success
)

const fadeDuration = 140 * time.Millisecond

func NewWithRuntime(ar *animation.Runtime) *Model {
	if ar == nil {
		panic("messagebar: nil animation runtime")
	}
	m := New()
	m.ar, m.transition = ar, ar.Transition()
	return m
}

func (m *Model) severityColor() color.Color {
	switch m.message.Severity {
	case Warning:
		return styles.Warning
	case Error:
		return styles.Error
	case Success:
		return styles.Success
	default:
		return styles.Info
	}
}

func (m *Model) startNotice() tea.Cmd {
	m.closing, m.alpha = false, 1
	if m.ar == nil {
		return nil
	}
	if m.width == 0 || m.height == 0 || (m.message.Text == "" && len(m.message.Actions) == 0) {
		m.transition.Cancel()
		return nil
	}
	m.alpha = 0
	return m.transition.Start(fadeDuration, animation.Linear)
}

// ClearMessage starts a finite exit; disappearing actions stop accepting input immediately.
func (m *Model) ClearMessage() tea.Cmd {
	defer m.prepareView()
	if m.closing {
		return nil
	}
	if m.ar == nil || m.width == 0 || m.height == 0 || m.alpha == 0 || (m.message.Text == "" && len(m.message.Actions) == 0) {
		m.StopAnimations()
		return nil
	}
	m.closing, m.closeFrom = true, m.alpha
	m.focused, m.hovered, m.selected = false, -1, -1
	m.invalidate()
	return m.transition.Start(fadeDuration, animation.Linear)
}

func (m *Model) StopAnimation() { m.StopAnimations() }

// StopAnimations clears immediately and releases the component's runtime lease.
func (m *Model) StopAnimations() {
	if m.ar != nil {
		m.transition.Cancel()
	}
	m.message = Message{}
	m.closing, m.alpha = false, 1
	m.focused, m.hovered, m.selected = false, -1, -1
	m.layout()
	m.invalidate()
	m.prepareView()
}

func (m *Model) tick(msg animation.TickMsg) tea.Cmd {
	if m.ar == nil || !m.transition.Running() {
		return nil
	}
	before := m.View()
	wasDirty := m.visualDirty
	m.transition.Tick()
	m.alpha = m.transition.Value()
	if m.closing {
		m.alpha = m.closeFrom * (1 - m.alpha)
		if !m.transition.Running() {
			m.StopAnimations()
		}
	}
	m.cacheValid = false
	m.prepareView()
	m.visualDirty = wasDirty || m.cachedView != before
	if m.cachedView != before {
		msg.MarkDirty()
	}
	return nil
}
