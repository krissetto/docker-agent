package animation

import tea "charm.land/bubbletea/v2"

// Subscription represents a component's subscription to animation ticks.
// It encapsulates the registration/unregistration lifecycle, making it
// easier to manage animation state correctly. Its owner must serialize lifecycle
// calls and bind a runtime before Start.
//
// Usage (bind animSub with NewSubscription(ar)). The program owner batches
// ar.Continue() into its returned command after Init and every Update, after
// all component lifecycle work; component Init alone does not schedule ticks.
//
//	type MyComponent struct {
//	    animSub animation.Subscription
//	}
//
//	func (m *MyComponent) Init() tea.Cmd {
//	    return m.animSub.Start()
//	}
//
//	func (m *MyComponent) Cleanup() {
//	    m.animSub.Stop()
//	}
type Subscription struct {
	active bool
	ar     *Runtime
}

// NewSubscription returns an inactive subscription owned by an animation runtime.
// Its program owner must schedule through Runtime.Continue after Init and Update.
func NewSubscription(ar *Runtime) Subscription {
	if ar == nil {
		panic("animation: nil runtime")
	}
	return Subscription{ar: ar}
}

// SetRuntime binds an inactive subscription to a program's animation runtime.
func (s *Subscription) SetRuntime(ar *Runtime) {
	if ar == nil {
		panic("animation: nil runtime")
	}
	if s.active {
		panic("animation: cannot rebind active subscription")
	}
	s.ar = ar
}

func (s *Subscription) boundRuntime() *Runtime {
	if s.ar == nil {
		panic("animation: unbound Subscription")
	}
	return s.ar
}

// Start activates the subscription if not already active. A runtime must be
// bound before Start. It only registers and returns nil; the program owner
// schedules through Runtime.Continue. Repeated calls only register once.
func (s *Subscription) Start() tea.Cmd {
	if s.active {
		return nil
	}
	ar := s.boundRuntime()
	cmd := ar.start()
	s.active = true
	return cmd
}

// Stop deactivates the subscription if currently active.
// Safe to call multiple times - only the first call unregisters.
func (s *Subscription) Stop() {
	if !s.active {
		return
	}
	s.active = false
	s.boundRuntime().Unregister()
}

// IsActive returns whether the subscription is currently active.
func (s *Subscription) IsActive() bool {
	return s.active
}

// Reset returns a new inactive subscription.
// Useful when recreating a component that needs fresh animation state.
func (s *Subscription) Reset() Subscription {
	ar := s.ar
	s.Stop()
	return Subscription{ar: ar}
}
