package tuitest

import (
	tea "charm.land/bubbletea/v2"
)

// Wheel sends steps mouse-wheel notches at screen coordinates x,y: positive
// steps scroll up and negative steps scroll down. Each notch is one real
// tea.MouseWheelMsg, as a terminal emits it.
func (d *Driver) Wheel(x, y, steps int) *Driver {
	button := tea.MouseWheelUp
	if steps < 0 {
		button = tea.MouseWheelDown
		steps = -steps
	}
	for range steps {
		d.sendSync(tea.MouseWheelMsg{X: x, Y: y, Button: button})
	}
	return d
}
