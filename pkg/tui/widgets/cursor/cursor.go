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
}
type BlinkMsg struct{ token uint64 }
type startMsg struct{}

var tokens atomic.Uint64

// Blink starts a fresh blink cycle when delivered to a focused widget.
func Blink() tea.Msg { return startMsg{} }

// Model is value-owned: copying it does not share mutable timer state.
type Model struct {
	token  uint64
	hidden bool
}

func (m Model) Visible(style Style) bool { return !style.Blink || !m.hidden }
func (m *Model) Stop()                   { m.token = tokens.Add(1); m.hidden = false }
func (m *Model) Reset(style Style) tea.Cmd {
	m.Stop()
	if !style.Blink {
		return nil
	}
	return m.tick(style)
}
func (m Model) tick(style Style) tea.Cmd {
	delay := style.BlinkSpeed
	if delay <= 0 {
		delay = 530 * time.Millisecond
	}
	token := m.token
	return tea.Tick(delay, func(time.Time) tea.Msg { return BlinkMsg{token: token} })
}
func (m *Model) Update(msg tea.Msg, style Style) tea.Cmd {
	switch msg := msg.(type) {
	case startMsg:
		return m.Reset(style)
	case BlinkMsg:
		if msg.token == 0 || msg.token != m.token || !style.Blink {
			return nil
		}
		m.hidden = !m.hidden
		m.token = tokens.Add(1)
		return m.tick(style)
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
