package animation

import (
	"context"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

// ownerLeaseScheduler exposes only actual executed timer commands. The program,
// not the test, executes every command returned by its Init/Update boundary.
type ownerLeaseScheduler struct {
	mu       sync.Mutex
	now      time.Time
	requests chan ownerLeaseRequest
	ctx      func() context.Context
}

type ownerLeaseRequest struct {
	at   time.Time
	fire chan time.Time
}

func (s *ownerLeaseScheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *ownerLeaseScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	request := ownerLeaseRequest{at: s.Now().Add(delay), fire: make(chan time.Time, 1)}
	return func() tea.Msg {
		select {
		case s.requests <- request:
		case <-s.ctx().Done():
			return nil
		}
		select {
		case at := <-request.fire:
			return create(at)
		case <-s.ctx().Done():
			return nil
		}
	}
}

func (s *ownerLeaseScheduler) deliver(request ownerLeaseRequest) {
	s.mu.Lock()
	s.now = request.at
	s.mu.Unlock()
	request.fire <- request.at
}

type ownerLeaseAction string

type ownerLeaseObservation struct {
	action   ownerLeaseAction
	accepted bool
	frames   int
	elapsed  time.Duration
	active   int32
}

type ownerLeaseProgram struct {
	runtime       *Runtime
	first, second Subscription
	observations  chan ownerLeaseObservation
	frames        int
}

func (m *ownerLeaseProgram) Init() tea.Cmd  { return m.runtime.Continue() }
func (m *ownerLeaseProgram) View() tea.View { return tea.NewView("owner") }
func (m *ownerLeaseProgram) Update(msg tea.Msg) (model tea.Model, cmd tea.Cmd) {
	defer func() { cmd = tea.Batch(cmd, m.runtime.Continue()) }()
	observation := ownerLeaseObservation{}
	switch msg := msg.(type) {
	case ownerLeaseAction:
		observation.action = msg
		switch msg {
		case "start":
			cmd = tea.Batch(m.first.Start(), m.second.Start())
		case "restart":
			m.first.Stop()
			m.second.Stop()
			cmd = tea.Batch(m.first.Start(), m.second.Start())
		case "stop", "quit":
			m.first.Stop()
			m.second.Stop()
			if msg == "quit" {
				m.runtime.Stop()
				cmd = tea.Quit
			}
		}
	case TickMsg:
		_, observation.accepted = m.runtime.Accept(msg)
		if observation.accepted {
			m.frames++
		}
	default:
		return m, nil
	}
	observation.frames = m.frames
	observation.elapsed = m.runtime.Now()
	observation.active = m.runtime.ActiveCount()
	m.observations <- observation
	return m, cmd
}

func ownerLeaseReceive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	synctest.Wait()
	select {
	case value := <-channel:
		return value
	default:
		t.Fatal("program failed to deliver the expected owner lifecycle event")
		var zero T
		return zero
	}
}

func ownerLeaseEmpty[T any](t *testing.T, channel <-chan T) {
	t.Helper()
	synctest.Wait()
	select {
	case value := <-channel:
		t.Fatalf("unexpected duplicate owner work: %+v", value)
	default:
	}
}

func TestOwnerLeaseActualProgramLifecycle(t *testing.T) {
	for _, staleFirst := range []bool{true, false} {
		name := "fresh-before-stale"
		if staleFirst {
			name = "stale-before-fresh"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				scheduler := &ownerLeaseScheduler{ctx: func() context.Context { return ctx }, now: time.Unix(1, 0), requests: make(chan ownerLeaseRequest, 8)}
				runtime := NewRuntimeWithScheduler(scheduler)
				model := &ownerLeaseProgram{runtime: runtime, first: runtime.Subscribe(), second: runtime.Subscribe(), observations: make(chan ownerLeaseObservation, 8)}
				// OS signal machinery lives outside the synctest bubble; this host
				// shuts down through program messages and context cancellation only.
				program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
				done := make(chan error, 1)
				go func() { _, err := program.Run(); done <- err }()
				t.Cleanup(program.Kill)
				action := func(value ownerLeaseAction) ownerLeaseObservation {
					program.Send(value)
					observation := ownerLeaseReceive(t, model.observations)
					require.Equal(t, value, observation.action)
					return observation
				}
				require.Zero(t, action("probe").active)
				ownerLeaseEmpty(t, scheduler.requests)
				require.EqualValues(t, 2, action("start").active)
				stale := ownerLeaseReceive(t, scheduler.requests)
				ownerLeaseEmpty(t, scheduler.requests)
				require.EqualValues(t, 2, action("restart").active)
				fresh := ownerLeaseReceive(t, scheduler.requests)
				ownerLeaseEmpty(t, scheduler.requests)
				if staleFirst {
					scheduler.deliver(stale)
					require.False(t, ownerLeaseReceive(t, model.observations).accepted)
					ownerLeaseEmpty(t, scheduler.requests)
				}
				scheduler.deliver(fresh)
				first := ownerLeaseReceive(t, model.observations)
				require.True(t, first.accepted)
				require.Equal(t, 1, first.frames)
				require.Equal(t, TickRate, first.elapsed)
				next := ownerLeaseReceive(t, scheduler.requests)
				if !staleFirst {
					scheduler.deliver(stale)
					rejected := ownerLeaseReceive(t, model.observations)
					require.False(t, rejected.accepted)
					require.Equal(t, first.elapsed, rejected.elapsed)
					ownerLeaseEmpty(t, scheduler.requests)
				}
				for frame := 2; frame <= 4; frame++ {
					scheduler.deliver(next)
					observation := ownerLeaseReceive(t, model.observations)
					require.True(t, observation.accepted)
					require.Equal(t, frame, observation.frames, "ongoing frames arrive without external wakeups")
					next = ownerLeaseReceive(t, scheduler.requests)
					ownerLeaseEmpty(t, scheduler.requests)
				}
				stopped := action("stop")
				require.Zero(t, stopped.active)
				scheduler.deliver(next)
				require.False(t, ownerLeaseReceive(t, model.observations).accepted)
				ownerLeaseEmpty(t, scheduler.requests)
				scheduler.mu.Lock()
				scheduler.now = scheduler.now.Add(time.Hour)
				scheduler.mu.Unlock()
				action("start")
				scheduler.deliver(ownerLeaseReceive(t, scheduler.requests))
				resumed := ownerLeaseReceive(t, model.observations)
				require.True(t, resumed.accepted)
				require.Equal(t, stopped.elapsed+TickRate, resumed.elapsed, "idle time is excluded")
				next = ownerLeaseReceive(t, scheduler.requests)
				action("stop")
				scheduler.deliver(next)
				require.False(t, ownerLeaseReceive(t, model.observations).accepted)
				require.Zero(t, action("quit").active)
				require.NoError(t, ownerLeaseReceive(t, done))
				ownerLeaseEmpty(t, scheduler.requests)
				require.Nil(t, runtime.Continue())
			})
		})
	}
}

func TestOwnerLeaseRegistrationReplayAndSnapshot(t *testing.T) {
	scheduler := &ownerLeaseScheduler{ctx: t.Context, now: time.Unix(1, 0), requests: make(chan ownerLeaseRequest, 8)}
	runtime := NewRuntimeWithScheduler(scheduler)
	first, second := runtime.Subscribe(), runtime.Transition()
	require.Nil(t, first.Start())
	require.Nil(t, second.Start(time.Second, Linear))
	require.EqualValues(t, 2, runtime.ActiveCount())
	require.False(t, runtime.tickScheduled, "component starts reserve no timer")
	cmd := runtime.Continue()
	require.NotNil(t, cmd)
	require.Nil(t, runtime.Continue(), "owner deduplicates an outstanding lease")
	// Deliver deterministic timestamps through the real scheduler command.
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	request := <-scheduler.requests
	scheduler.deliver(request)
	tick := (<-result).(TickMsg)
	_, accepted := runtime.Accept(tick)
	require.True(t, accepted)
	elapsed := runtime.Now()
	successor := runtime.Continue()
	require.NotNil(t, successor)
	_, accepted = runtime.Accept(tick)
	require.False(t, accepted, "replaying an accepted tick cannot consume a newly armed lease")
	require.Equal(t, elapsed, runtime.Now())
	require.Nil(t, runtime.Continue(), "replay leaves the successor lease intact")
	first.Stop()
	second.Cancel()
	go func() { result <- successor() }()
	scheduler.deliver(<-scheduler.requests)
	_, accepted = runtime.Accept((<-result).(TickMsg))
	require.False(t, accepted, "shutdown invalidates the executed queued successor")
	require.Nil(t, runtime.Continue())

	snapshot := NewSnapshotRuntime(5 * time.Second)
	sub, transition := snapshot.Subscribe(), snapshot.Transition()
	require.Nil(t, sub.Start())
	require.Nil(t, transition.Start(time.Second, Linear))
	for range 20 {
		_ = transition.Value()
		_ = snapshot.Now()
	}
	require.Equal(t, 5*time.Second, snapshot.Now())
	require.False(t, snapshot.tickScheduled, "snapshot observations and starts never schedule work")
	sub.Stop()
	transition.Cancel()
	require.Nil(t, snapshot.Continue())
}
