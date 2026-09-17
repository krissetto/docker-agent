// Package animation provides centralized animation tick management for the TUI.
// All animated components (spinners, fades, etc.) share a single tick stream
// to avoid tick storms and ensure synchronized animations.
//
// Runtime methods synchronize access to runtime state. Subscription and
// Transition lifecycles must be serialized by their owner, typically through
// Bubble Tea's Init and Update loop.
package animation

import (
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TickMsg is broadcast to all animated components on each animation frame.
// Components should handle this message to update their animation state.
type TickMsg struct {
	// Frame is retained for compatibility with components that have not yet
	// adopted elapsed-time animation. It counts this runtime's accepted ticks.
	Frame int

	runtimeIdentity *runtimeIdentity
	generation      int // identifies the current tick lease

	// deliveredAt is the timestamp tea.Tick supplies when this timer fires.
	deliveredAt time.Time
	// timerStartedAt records when the animation runtime created this tick's timer. It is
	// the first tick's elapsed-time baseline; later ticks use lastDeliveredAt.
	timerStartedAt    time.Time
	dirty             *atomic.Bool
	elapsedBeforeTick time.Duration
	elapsedAfterTick  time.Duration
}

// MarkDirty records that this accepted tick changed visible output. TickMsg
// copies share the marker, so root can decide after complete fanout whether a
// new view must be composed.
func (m TickMsg) MarkDirty() {
	if m.dirty != nil {
		m.dirty.Store(true)
	}
}

// Dirty reports whether any visible component marked this accepted tick dirty.
func (m TickMsg) Dirty() bool { return m.dirty != nil && m.dirty.Load() }

// ElapsedBounds returns the animation clock immediately before and after this
// accepted tick.
func (m TickMsg) ElapsedBounds() (time.Duration, time.Duration) {
	return m.elapsedBeforeTick, m.elapsedAfterTick
}

// Scheduler provides the wall clock and delayed message delivery used by a
// runtime. Embedders may supply a deterministic scheduler while preserving the
// exact production tick lease and acceptance path.
type Scheduler interface {
	Now() time.Time
	Tick(delay time.Duration, createMsg func(time.Time) tea.Msg) tea.Cmd
}

type wallScheduler struct{}

func (wallScheduler) Now() time.Time                                          { return time.Now() }
func (wallScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd { return tea.Tick(d, f) }

// Runtime owns a program's animation registrations, clock, and single tick lease.
// Components register work synchronously; the program owner must return Continue's
// command after Init and every Update, after all component lifecycle work, and
// Accept each delivered TickMsg before forwarding it to components.
type Runtime struct {
	mu              sync.Mutex
	runtimeIdentity *runtimeIdentity
	elapsed         time.Duration
	active          int32
	generation      int

	tickScheduled   bool
	lastDeliveredAt time.Time
	acceptedTicks   uint64
	scheduler       Scheduler
}

type runtimeIdentity struct{ _ byte }

// NewRuntime creates an isolated program-scoped animation runtime. Its owner
// schedules ticks through Continue; starting component animations never schedules.
func NewRuntime() *Runtime { return NewRuntimeWithScheduler(wallScheduler{}) }

// NewRuntimeWithScheduler creates a runtime using the supplied production
// scheduling boundary. Tick creation, leases, and acceptance remain owned by
// Runtime; only time acquisition and delayed delivery are delegated.
func NewRuntimeWithScheduler(s Scheduler) *Runtime {
	if s == nil {
		panic("animation: nil Scheduler")
	}
	return &Runtime{runtimeIdentity: &runtimeIdentity{}, scheduler: s}
}

// NewSnapshotRuntime supplies a fixed elapsed time for snapshot rendering, such
// as the lean TUI's current frame. Construct components and call View without
// scheduling through Continue or delivering ticks through Accept.
func NewSnapshotRuntime(elapsed time.Duration) *Runtime {
	r := NewRuntime()
	r.elapsed = max(elapsed, 0)
	return r
}

// Register increments this animation runtime's active animation count.
func (r *Runtime) Register() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active++
}

// Unregister decrements this animation runtime's active animation count.
func (r *Runtime) Unregister() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active > 0 {
		r.active--
	}
	if r.active == 0 {
		r.abandonLeaseLocked()
		r.lastDeliveredAt = time.Time{}
	}
}

// HasActive reports whether this animation runtime has active animations.
func (r *Runtime) HasActive() bool { return r.ActiveCount() > 0 }

// ActiveCount returns this animation runtime's active animation count.
func (r *Runtime) ActiveCount() int32 {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Continue schedules a tick only when animations are active and no tick is
// outstanding. The program owner calls it after Init and every Update, after
// component lifecycle postprocessing (including non-tick and early-return paths),
// and must preserve the returned command. Paused owners skip Continue until they
// resume; components only register work and never call Continue themselves.
func (r *Runtime) Continue() tea.Cmd {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == 0 {
		r.abandonLeaseLocked()
		r.lastDeliveredAt = time.Time{}
		return nil
	}
	return r.tickLocked()
}

// Accept consumes this animation runtime's current tick lease and advances its clock.
func (r *Runtime) Accept(msg TickMsg) (TickMsg, bool) {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg.runtimeIdentity != r.runtimeIdentity || msg.generation != r.generation || !r.tickScheduled {
		return msg, false
	}
	r.acceptedTicks++
	// Frame is the int representation of the monotonically increasing tick count.
	msg.Frame = int(r.acceptedTicks) //nolint:gosec // Compatibility field is intentionally int.
	r.tickScheduled = false
	delta := msg.deliveredAt.Sub(r.lastDeliveredAt)
	if r.lastDeliveredAt.IsZero() {
		delta = msg.deliveredAt.Sub(msg.timerStartedAt)
	}
	if delta <= 0 {
		delta = TickRate
	}
	r.lastDeliveredAt = msg.deliveredAt
	msg.elapsedBeforeTick = r.elapsed
	r.elapsed += delta
	msg.elapsedAfterTick = r.elapsed
	msg.dirty = &atomic.Bool{}
	return msg, true
}

// Now returns elapsed time accepted by this animation runtime.
func (r *Runtime) Now() time.Duration {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.elapsed
}

// Stop invalidates all queued ticks and releases every registration owned by
// this program. Program teardown calls it after component cleanup as a final
// ownership boundary; a late queued TickMsg can then never revive the chain.
func (r *Runtime) Stop() {
	r.mustExist()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = 0
	r.abandonLeaseLocked()
	r.lastDeliveredAt = time.Time{}
}

// Subscribe creates an inactive subscription owned by this animation runtime.
func (r *Runtime) Subscribe() Subscription { return NewSubscription(r) }

// Transition creates an idle transition owned by this animation runtime.
func (r *Runtime) Transition() Transition { return NewTransition(r) }

func (r *Runtime) start() tea.Cmd {
	r.Register()
	return nil
}

func (r *Runtime) mustExist() {
	if r == nil || r.runtimeIdentity == nil {
		panic("animation: nil or zero Runtime")
	}
}

func (r *Runtime) abandonLeaseLocked() {
	if r.tickScheduled {
		r.generation++
		r.tickScheduled = false
	}
}

func (r *Runtime) tickLocked() tea.Cmd {
	if r.tickScheduled {
		return nil
	}
	// Each allocation gets its own token: replaying an accepted tick must not
	// consume the successor lease after the owner has continued.
	r.generation++
	r.tickScheduled = true
	runtimeIdentity, generation, timerStartedAt := r.runtimeIdentity, r.generation, r.scheduler.Now()
	return r.scheduler.Tick(TickRate, func(t time.Time) tea.Msg {
		return TickMsg{runtimeIdentity: runtimeIdentity, generation: generation, deliveredAt: t, timerStartedAt: timerStartedAt}
	})
}

// TickRate is the shared interval between animation ticks.
const TickRate = time.Second / 60
