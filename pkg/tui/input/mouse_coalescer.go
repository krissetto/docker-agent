package input

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

const mouseFlushInterval = 16 * time.Millisecond

// MouseCoalescer batches motion and wheel input on a 16 ms cadence.
// Clicks and releases flush pending input first.
type MouseCoalescer struct {
	mu sync.Mutex

	latestMotion  tea.MouseMotionMsg
	hasMotion     bool
	wheelDelta    int
	hasWheel      bool
	pointerX      int
	pointerY      int
	timerPending  bool
	timerSequence uint64
	timer         *time.Timer
	stopped       bool

	sender func(tea.Msg)
}

type mouseFlushMsg struct{ sequence uint64 }

// NewMouseCoalescer creates a mouse input coalescer.
func NewMouseCoalescer() *MouseCoalescer { return &MouseCoalescer{} }

// SetSender sets the function used to send coalesced updates.
func (c *MouseCoalescer) SetSender(sender func(tea.Msg)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sender = sender
}

// Filter coalesces motion and wheel messages. It returns clicks and releases
// immediately, with pending input ordered before them.
func (c *MouseCoalescer) Filter(msg tea.Msg) tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		if _, wake := msg.(mouseFlushMsg); wake {
			return nil
		}
		return msg
	}
	switch msg := msg.(type) {
	case mouseFlushMsg:
		if msg.sequence != c.timerSequence {
			return nil
		}
		update, ok := c.takeLocked()
		if !ok {
			return nil
		}
		return update
	case tea.BlurMsg:
		c.discardLocked()
		return msg
	case tea.MouseWheelMsg:
		delta, ok := wheelDelta(msg)
		if !ok {
			return msg
		}
		c.wheelDelta += delta
		c.hasWheel = true
		c.pointerX, c.pointerY = msg.X, msg.Y
		// Wheel input supplies the latest pointer position.
		c.hasMotion = false
		c.scheduleLocked()
		return nil

	case tea.MouseMotionMsg:
		if c.hasWheel {
			// Wheel position takes precedence within the current interval.
			return nil
		}
		c.latestMotion = msg
		c.hasMotion = true
		c.pointerX, c.pointerY = msg.X, msg.Y
		c.scheduleLocked()
		return nil

	case tea.MouseClickMsg:
		return c.boundaryLocked(msg)

	case tea.MouseReleaseMsg:
		return c.boundaryLocked(msg)

	case tea.WindowSizeMsg:
		return c.boundaryLocked(msg)
	}
	return msg
}

func (c *MouseCoalescer) scheduleLocked() {
	if c.timerPending {
		return
	}
	c.timerPending = true
	c.timerSequence++
	sequence := c.timerSequence
	c.timer = time.AfterFunc(mouseFlushInterval, func() { c.flush(sequence) })
}

func (c *MouseCoalescer) boundaryLocked(event tea.Msg) tea.Msg {
	update, ok := c.takeLocked()
	if !ok {
		return event
	}
	c.timerSequence++
	return messages.PointerBoundaryMsg{Pending: update, Event: event}
}

// The timer only wakes the event loop. Taking input here would race a click
// already waiting in the program and could deliver its preceding motion late.
func (c *MouseCoalescer) flush(sequence uint64) {
	c.mu.Lock()
	sender, valid := c.sender, !c.stopped && sequence == c.timerSequence
	c.mu.Unlock()
	if valid && sender != nil {
		sender(mouseFlushMsg{sequence: sequence})
	}
}

func (c *MouseCoalescer) discardLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.timerSequence++
	c.hasMotion, c.hasWheel, c.timerPending = false, false, false
	c.wheelDelta = 0
}

// Stop releases pending timers without delivering input to a closed program.
func (c *MouseCoalescer) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.sender = nil
	c.discardLocked()
}

func (c *MouseCoalescer) takeLocked() (messages.PointerUpdateMsg, bool) {
	if !c.timerPending && !c.hasMotion && c.wheelDelta == 0 {
		return messages.PointerUpdateMsg{}, false
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	update := messages.PointerUpdateMsg{
		X:          c.pointerX,
		Y:          c.pointerY,
		WheelDelta: c.wheelDelta,
		HasWheel:   c.hasWheel,
	}
	if c.hasMotion {
		motion := c.latestMotion
		update.Motion = &motion
	}
	c.hasMotion = false
	c.wheelDelta = 0
	c.hasWheel = false
	c.timerPending = false
	return update, update.Motion != nil || update.HasWheel
}

func wheelDelta(msg tea.MouseWheelMsg) (int, bool) {
	switch msg.Button {
	case tea.MouseWheelUp:
		return -1, true
	case tea.MouseWheelDown:
		return 1, true
	default:
		return 0, false
	}
}
