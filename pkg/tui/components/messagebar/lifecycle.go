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
		if m.message.Category == Cancellation {
			return styles.Info
		}
		return styles.TextSecondary
	}
}

func (m *Model) startNotice() tea.Cmd {
	from := 0.0
	if m.ar != nil && m.transition.Running() {
		from = m.alpha
	}
	m.closing, m.alpha = false, 1
	if m.ar == nil {
		return nil
	}
	if m.width == 0 || m.height == 0 || m.message.empty() {
		m.transition.Cancel()
		return nil
	}
	m.entryFrom, m.alpha = from, from
	return m.transition.Start(fadeDuration, animation.Linear)
}

// ClearOwned dismisses only the accepted revision owned by token. Explicit
// owner dismissal is allowed regardless of priority; stale timers are not.
func (m *Model) ClearOwned(token Token) tea.Cmd {
	if token.Generation == 0 || token != m.token {
		return nil
	}
	return m.ClearMessage()
}

// Expire is clock-free and ignores early, persistent, or stale deadlines.
func (m *Model) Expire(token Token, now time.Time) tea.Cmd {
	if m.deadline.IsZero() || now.Before(m.deadline) {
		return nil
	}
	return m.ClearOwned(token)
}

// ClearMessage is an explicit unconditional dismissal, not an expiry callback.
// It starts a finite exit; disappearing actions stop accepting input immediately.
func (m *Model) ClearMessage() tea.Cmd {
	defer m.prepareView()
	m.token, m.deadline = Token{}, time.Time{}
	if m.closing {
		return nil
	}
	if m.ar == nil || m.width == 0 || m.height == 0 || m.alpha == 0 || m.message.empty() {
		m.StopAnimations()
		return nil
	}
	m.closing, m.closeFrom = true, m.alpha
	m.focused, m.hovered, m.selected = false, -1, -1
	m.invalidate()
	return m.transition.Start(fadeDuration, animation.Linear)
}

// SettledPresentation reports applied local presentation state without advancing
// the transition or consulting other components' leases and pending commands.
// Empty and hidden rows are settled; an elapsed transition still needs its Tick.
func (m *Model) SettledPresentation() bool {
	return !m.transition.Running() && !m.closing && m.alpha == 1
}

func (m *Model) StopAnimation() { m.StopAnimations() }

// StopAnimations clears immediately and releases the component's runtime lease.
func (m *Model) StopAnimations() {
	if m.ar != nil {
		m.transition.Cancel()
	}
	m.message = Message{}
	m.token, m.deadline = Token{}, time.Time{}
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
	m.alpha = m.entryFrom + (1-m.entryFrom)*m.transition.Value()
	if m.closing {
		m.alpha = m.closeFrom * (1 - m.transition.Value())
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
