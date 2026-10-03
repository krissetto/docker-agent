package tools

import "sync"

// ChangeSubscriber exposes independently owned tool-list subscriptions.
type ChangeSubscriber interface{ SubscribeToolsChanged(handler func()) func() }

// Subscribers is a concurrency-safe fan-out registry for host callbacks on
// toolsets shared by several runtimes or streams. Notify snapshots the set
// under the lock and invokes callbacks outside it, so a callback may
// subscribe or unsubscribe re-entrantly. The zero value is ready to use.
type Subscribers[T any] struct {
	mu     sync.Mutex
	subs   map[uint64]func(T)
	nextID uint64
}

// Subscribe registers cb and returns an idempotent unsubscribe function.
// A nil cb registers nothing.
func (s *Subscribers[T]) Subscribe(cb func(T)) (unsubscribe func()) {
	if cb == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = make(map[uint64]func(T))
	}
	id := s.nextID
	s.nextID++
	s.subs[id] = cb
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// Notify invokes every current subscriber with v.
func (s *Subscribers[T]) Notify(v T) {
	s.mu.Lock()
	subs := make([]func(T), 0, len(s.subs))
	for _, cb := range s.subs {
		subs = append(subs, cb)
	}
	s.mu.Unlock()

	for _, cb := range subs {
		cb(v)
	}
}

// ChangeSubscribers backs ChangeSubscriber implementations: a Subscribers
// registry for argument-less tools-changed handlers.
type ChangeSubscribers struct {
	subs Subscribers[struct{}]
}

func (c *ChangeSubscribers) Subscribe(handler func()) (unsubscribe func()) {
	if handler == nil {
		return func() {}
	}
	return c.subs.Subscribe(func(struct{}) { handler() })
}

func (c *ChangeSubscribers) Notify() {
	c.subs.Notify(struct{}{})
}
