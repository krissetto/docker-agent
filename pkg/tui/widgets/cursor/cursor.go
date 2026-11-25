// Package cursor manages virtual cursor timing independently of text editing.
package cursor

import (
	"image/color"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Style retains the terminal cursor attributes and virtual cursor timing.
type Style struct {
	Color      color.Color
	Shape      tea.CursorShape
	Blink      bool
	BlinkSpeed time.Duration
	// HostManaged starts timing only when the host exposes this widget.
	HostManaged bool
}
type BlinkMsg struct{ token uint64 }
type startMsg struct{}

// Input is the narrow cursor surface of a text widget. The host selects the
// exposed input; the widget owns timing and invalidates its presentation.
type Input interface {
	Focused() bool
	SetCursorExposed(bool) (tea.Cmd, bool)
	ApplyCursorBlink(BlinkMsg) (tea.Cmd, bool)
	WakeCursor() bool
}

// Of returns the current text input of a component, if it has one.
func Of(component any) Input {
	if owner, ok := component.(interface{ CursorInput() Input }); ok {
		return owner.CursorInput()
	}
	return nil
}

var tokens atomic.Uint64

// Blink starts a fresh blink cycle when delivered to a focused widget.
func Blink() tea.Msg { return startMsg{} }

// Model is value-owned: copying it does not share mutable timer state.
type Model struct {
	token            uint64
	deadline         time.Time
	hidden           bool
	managed, exposed bool
}

func (m Model) Visible(style Style) bool {
	return (!m.managed || m.exposed) && (!style.Blink || !m.hidden)
}

func (m *Model) Stop() { m.token = 0; m.hidden = false }

// Blur leaves root-owned scheduling intact until the host reconciles focus.
// This also covers a blur/refocus pair within one host update.
func (m *Model) Blur() {
	if !m.managed {
		m.Stop()
	}
}

// Expose retires the old cycle or begins a fresh visible interval. Exposure
// does not change logical focus or switch between native and virtual cursors.
func (m *Model) Expose(exposed bool, style Style) tea.Cmd {
	m.managed, m.exposed = true, exposed
	m.Stop()
	if !exposed || !style.Blink {
		return nil
	}
	return m.start(style)
}

func (m *Model) Reset(style Style) tea.Cmd {
	if m.managed || style.HostManaged {
		m.Wake(style)
		return nil
	}
	m.Stop()
	if !style.Blink {
		return nil
	}
	return m.start(style)
}

// Wake shows the caret without replacing the timer already in flight.
func (m *Model) Wake(style Style) {
	if m.token != 0 {
		m.hidden = false
		m.deadline = time.Now().Add(interval(style))
	}
}

func interval(style Style) time.Duration {
	if style.BlinkSpeed > 0 {
		return style.BlinkSpeed
	}
	return 500 * time.Millisecond
}

func (m *Model) start(style Style) tea.Cmd {
	m.deadline = time.Now().Add(interval(style))
	return m.tick()
}

func (m *Model) tick() tea.Cmd {
	token := tokens.Add(1)
	m.token = token
	return tea.Tick(time.Until(m.deadline), func(time.Time) tea.Msg { return BlinkMsg{token: token} })
}

func (m *Model) Update(msg tea.Msg, style Style) tea.Cmd {
	switch msg := msg.(type) {
	case startMsg:
		return m.Reset(style)
	case BlinkMsg:
		if msg.token == 0 || msg.token != m.token || !style.Blink {
			return nil
		}
		if !time.Now().Before(m.deadline) {
			m.hidden = !m.hidden
			m.deadline = time.Now().Add(interval(style))
		}
		return m.tick()
	}
	return nil
}

// Render decorates a complete grapheme, never a partial byte or rune sequence.
func Render(value string, style Style) string {
	s := lipgloss.NewStyle().Reverse(true)
	if style.Color != nil {
		s = s.Foreground(style.Color)
	}
	return s.Render(value)
}
