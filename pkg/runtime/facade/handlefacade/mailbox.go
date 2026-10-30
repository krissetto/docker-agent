package handlefacade

import (
	"sync"

	"github.com/docker/docker-agent/pkg/runtime/facade"
)

// mailbox decouples a producer that must never block (the session feed) from a
// consumer that may stop reading. Pushes are unbounded; the pump goroutine
// delivers in order and closes the output exactly once after close() and drain.
type mailbox struct {
	mu     sync.Mutex
	queue  []facade.Event
	done   bool
	wake   chan struct{}
	out    chan facade.Event
	closed sync.Once
}

func newMailbox() *mailbox {
	m := &mailbox{wake: make(chan struct{}, 1), out: make(chan facade.Event)}
	go m.pump()
	return m
}

func (m *mailbox) push(ev facade.Event) {
	m.mu.Lock()
	if !m.done {
		m.queue = append(m.queue, ev)
	}
	m.mu.Unlock()
	m.signal()
}

// finish stops accepting events; the output closes after queued events are delivered.
func (m *mailbox) finish() {
	m.mu.Lock()
	m.done = true
	m.mu.Unlock()
	m.signal()
}

func (m *mailbox) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *mailbox) pump() {
	for {
		m.mu.Lock()
		if len(m.queue) > 0 {
			ev := m.queue[0]
			m.queue[0] = facade.Event{}
			m.queue = m.queue[1:]
			m.mu.Unlock()
			m.out <- ev
			continue
		}
		if m.done {
			m.mu.Unlock()
			m.closed.Do(func() { close(m.out) })
			return
		}
		m.mu.Unlock()
		<-m.wake
	}
}
