package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

type sessionStub struct {
	observations chan runtime.Observation
	mu           sync.Mutex
	since        []*uint64
}

func (*sessionStub) ID() string                        { return "s" }
func (*sessionStub) AgentName() string                 { return "a" }
func (*sessionStub) Metadata() runtime.SessionMetadata { return runtime.SessionMetadata{} }
func (*sessionStub) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (*sessionStub) Retry(context.Context) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (*sessionStub) Steer(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (*sessionStub) Send(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{}, nil
}

func (a *sessionStub) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	a.mu.Lock()
	a.since = append(a.since, options.Since)
	a.mu.Unlock()
	select {
	case o := <-a.observations:
		return o, nil
	case <-ctx.Done():
		return runtime.Observation{}, ctx.Err()
	}
}

func (*sessionStub) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{}, nil
}
func (*sessionStub) Respond(context.Context, runtime.InteractionResponse) error { return nil }
func (*sessionStub) UpdateTitle(context.Context, string) error                  { return nil }
func (*sessionStub) Cancel(context.Context, string) (runtime.CancelResult, error) {
	return runtime.CancelResult{}, nil
}
func (*sessionStub) Release(context.Context) error { return nil }

type recordingSink struct {
	mu      sync.Mutex
	resets  []uint64
	applied []uint64
	pending []string
	errors  []error
	block   chan struct{}
	entered chan struct{}
}

func (s *recordingSink) Reset(v runtime.SessionSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets = append(s.resets, v.Cursor)
	s.pending = s.pending[:0]
	for _, pending := range v.PendingInputs {
		s.pending = append(s.pending, pending.TurnID)
	}
}

func (s *recordingSink) Apply(v runtime.SessionEvent) {
	if s.entered != nil {
		close(s.entered)
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, v.Sequence)
}

func (s *recordingSink) OnError(e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, e)
}

func obs(cursor uint64, replay []runtime.SessionEvent, events chan runtime.SessionEvent) runtime.Observation {
	return runtime.Observation{Initial: []runtime.SessionSnapshot{runtime.SessionSnapshot{Cursor: cursor}}, Replay: replay, Events: events, Cancel: func() {}}
}

func TestSnapshotResetCarriesCanonicalPendingFIFO(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 1)}
	events := make(chan runtime.SessionEvent)
	close(events)
	a.observations <- runtime.Observation{
		Initial: []runtime.SessionSnapshot{runtime.SessionSnapshot{PendingInputs: []runtime.PendingInput{{TurnID: "one"}, {TurnID: "two"}}}},
		Events:  events, Cancel: func() {},
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.pending) == 2
	}, time.Second, time.Millisecond)
	attachment.Detach()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	assert.Equal(t, []string{"one", "two"}, sink.pending)
}

func TestIndependentClientsObserveSameJournalOrder(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 2)}
	for range 2 {
		ch := make(chan runtime.SessionEvent, 1)
		ch <- runtime.SessionEvent{Sequence: 7}
		close(ch)
		a.observations <- obs(5, []runtime.SessionEvent{{Sequence: 5}, {Sequence: 6}}, ch)
	}
	s1, s2 := &recordingSink{}, &recordingSink{}
	x, _ := Attach(t.Context(), a, s1)
	y, _ := Attach(t.Context(), a, s2)
	require.Eventually(t, func() bool {
		s1.mu.Lock()
		first := len(s1.applied)
		s1.mu.Unlock()
		s2.mu.Lock()
		second := len(s2.applied)
		s2.mu.Unlock()
		return first == 2 && second == 2
	}, time.Second, time.Millisecond)
	x.Detach()
	y.Detach()
	s1.mu.Lock()
	defer s1.mu.Unlock()
	s2.mu.Lock()
	defer s2.mu.Unlock()
	assert.Equal(t, []uint64{6, 7}, s1.applied)
	assert.Equal(t, s1.applied, s2.applied)
}

func TestGapResetsFreshAndBoundsRetry(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 2)}
	gap := make(chan runtime.SessionEvent, 1)
	gap <- runtime.SessionEvent{Gap: true}
	close(gap)
	a.observations <- obs(1, nil, gap)
	fresh := make(chan runtime.SessionEvent, 1)
	fresh <- runtime.SessionEvent{Sequence: 3}
	close(fresh)
	a.observations <- obs(2, nil, fresh)
	s := &recordingSink{}
	x, _ := Attach(t.Context(), a, s)
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.applied) == 1 }, time.Second, time.Millisecond)
	x.Detach()
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Equal(t, []uint64{1, 2}, s.resets)
	assert.Equal(t, []uint64{3}, s.applied)
}

func TestObservationTerminalErrorsReconnectWithCursorAndExhaust(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 4)}
	for i := range 4 {
		events := make(chan runtime.SessionEvent)
		errs := make(chan error, 1)
		errs <- errors.New("dropped")
		close(errs)
		close(events)
		a.observations <- runtime.Observation{Initial: []runtime.SessionSnapshot{runtime.SessionSnapshot{Cursor: uint64(i + 1)}}, Events: events, Errors: errs, Cancel: func() {}}
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.errors) == 1 }, 2*time.Second, time.Millisecond)
	attachment.Detach()
	a.mu.Lock()
	defer a.mu.Unlock()
	require.Len(t, a.since, 4)
	assert.Nil(t, a.since[0])
	for i := 1; i < 4; i++ {
		require.NotNil(t, a.since[i])
		assert.Equal(t, uint64(i), *a.since[i])
	}
}

func TestObservationRepeatedGapExhaustionCallsOnError(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 4)}
	for range 4 {
		events := make(chan runtime.SessionEvent, 1)
		events <- runtime.SessionEvent{Gap: true}
		close(events)
		a.observations <- obs(1, nil, events)
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.errors) == 1 }, time.Second, time.Millisecond)
	attachment.Detach()
}

func TestDetachSynchronouslyFencesCallback(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 1)}
	ch := make(chan runtime.SessionEvent, 1)
	ch <- runtime.SessionEvent{Sequence: 1}
	a.observations <- obs(0, nil, ch)
	block := make(chan struct{})
	entered := make(chan struct{})
	s := &recordingSink{block: block, entered: entered}
	x, _ := Attach(t.Context(), a, s)
	<-entered
	done := make(chan struct{})
	go func() { x.Detach(); close(done) }()
	select {
	case <-done:
		t.Fatal("detach returned during callback")
	case <-time.After(10 * time.Millisecond):
	}
	close(block)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detach did not fence")
	}
}

func TestAttachRejectsTreeObservation(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 1)}
	a.observations <- runtime.Observation{
		Initial:       []runtime.SessionSnapshot{{}, {}},
		SessionsAdded: make(chan runtime.SessionSnapshot),
		Cancel:        func() {},
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.errors) == 1
	}, time.Second, time.Millisecond)
	attachment.Detach()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var treeErr *TreeObservationError
	assert.ErrorAs(t, sink.errors[0], &treeErr)
	assert.Empty(t, sink.resets)
}
