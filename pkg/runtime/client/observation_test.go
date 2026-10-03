package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
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
	return runtime.Observation{Initial: []runtime.SessionSnapshot{{Cursor: cursor}}, Replay: replay, Events: events, Cancel: func() {}}
}

func TestSnapshotResetCarriesCanonicalPendingFIFO(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 1)}
	events := make(chan runtime.SessionEvent)
	close(events)
	a.observations <- runtime.Observation{
		Initial: []runtime.SessionSnapshot{{PendingInputs: []runtime.PendingInput{{TurnID: "one"}, {TurnID: "two"}}}},
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

func TestObservationDisconnectsReconnectBeyondFourWithCursor(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 8)}
	for i := range 8 {
		events := make(chan runtime.SessionEvent)
		close(events)
		a.observations <- obs(uint64(i+1), []runtime.SessionEvent{{Sequence: uint64(i + 2)}}, events)
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return len(sink.applied) == 8 }, 2*time.Second, time.Millisecond)
	attachment.Detach()
	a.mu.Lock()
	defer a.mu.Unlock()
	require.GreaterOrEqual(t, len(a.since), 8)
	assert.Nil(t, a.since[0])
	for i := 1; i < 8; i++ {
		require.NotNil(t, a.since[i])
		assert.Equal(t, uint64(i+1), *a.since[i])
	}
	assert.Empty(t, sink.errors)
}

func TestObservationReconnectRetainsZeroCursor(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 4)}
	for range 4 {
		events := make(chan runtime.SessionEvent)
		close(events)
		a.observations <- obs(0, nil, events)
	}
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), a, sink)
	require.NoError(t, err)
	require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.since) >= 5 }, 2*time.Second, time.Millisecond)
	attachment.Detach()
	require.Len(t, a.since, 5)
	assert.Nil(t, a.since[0])
	for _, since := range a.since[1:] {
		require.NotNil(t, since)
		assert.Zero(t, *since)
	}
	assert.Equal(t, []uint64{0}, sink.resets)
	assert.Empty(t, sink.errors)
}

func TestObservationRepeatedGapsTerminateAfterBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		return obs(1, []runtime.SessionEvent{{Gap: true}}, nil), nil
	})
	sink := &recordingSink{}
	attachment := &Attachment{done: make(chan struct{})}
	var attempts []int
	attachment.runWithRetry(ctx, observer, sink, observationRetryPolicy{
		now: time.Now,
		wait: func(_ context.Context, attempt int) bool {
			attempts = append(attempts, attempt)
			if len(attempts) == 12 {
				cancel()
				return false
			}
			return true
		},
	})
	assert.Equal(t, 4, calls)
	assert.Equal(t, []int{0, 1, 2}, attempts)
	require.Len(t, sink.errors, 1)
	var gap *RepeatedObservationGapError
	require.ErrorAs(t, sink.errors[0], &gap)
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

type transcriptSink struct {
	recordingSink

	session *session.Session
	live    string
	deltas  []string
}

func (s *transcriptSink) Reset(snapshot runtime.SessionSnapshot) {
	s.recordingSink.Reset(snapshot)
	s.session = snapshot.Session.Clone()
	s.live = ""
}

func (s *transcriptSink) Apply(envelope runtime.SessionEvent) {
	s.recordingSink.Apply(envelope)
	switch event := envelope.Event.(type) {
	case *runtime.AgentChoiceEvent:
		s.live += event.Content
		s.deltas = append(s.deltas, event.Content)
	case *runtime.MessageAddedEvent:
		s.session.AddMessage(event.Message)
		s.live = ""
	}
}

func TestObservationReconnectPreservesStreamingAndCommittedTranscript(t *testing.T) {
	committed := session.New(session.WithID("s"))
	committed.AddMessage(session.NewAgentMessage("a", &chat.Message{Role: chat.MessageRoleAssistant, Content: "earlier"}))
	first := runtime.SessionEvent{Sequence: 11, Event: runtime.AgentChoice("a", "s", "first ")}
	second := runtime.SessionEvent{Sequence: 12, Event: runtime.AgentChoice("a", "s", "second")}
	message := session.NewAgentMessage("a", &chat.Message{Role: chat.MessageRoleAssistant, Content: "first second"})
	commit := runtime.SessionEvent{Sequence: 13, TranscriptPosition: 1, Event: &runtime.MessageAddedEvent{Message: message, SessionPosition: 1}}
	closed := make(chan runtime.SessionEvent)
	close(closed)
	sink := &transcriptSink{}
	initial := obs(10, []runtime.SessionEvent{first}, closed)
	initial.Initial[0].Session = committed.Clone()
	result := projectObservation(t.Context(), sink, initial, nil)
	require.Error(t, result.err)
	require.Equal(t, uint64(11), result.cursor)
	require.Equal(t, "first ", sink.live)
	require.Equal(t, 1, sink.session.MessageCount())

	reconnect := obs(12, []runtime.SessionEvent{first, second, second}, closed)
	reconnect.Initial[0].Session = committed.Clone()
	result = projectObservation(t.Context(), sink, reconnect, &result.cursor)
	require.Error(t, result.err)
	require.Equal(t, uint64(12), result.cursor)
	require.Equal(t, "first second", sink.live)
	require.Equal(t, []uint64{10}, sink.resets)
	require.Equal(t, 1, sink.session.MessageCount())

	// A later snapshot includes the commit; replay must not append it twice.
	committed.AddMessage(message)
	reconnect = obs(13, []runtime.SessionEvent{second, commit, commit}, closed)
	reconnect.Initial[0].Session = committed.Clone()
	reconnect.Initial[0].TranscriptPosition = 2
	result = projectObservation(t.Context(), sink, reconnect, &result.cursor)
	require.Error(t, result.err)
	assert.Equal(t, uint64(13), result.cursor)
	assert.Equal(t, []uint64{10}, sink.resets)
	assert.Equal(t, []uint64{11, 12, 13}, sink.applied)
	assert.Equal(t, []string{"first ", "second"}, sink.deltas)
	assert.Empty(t, sink.live)
	assert.Equal(t, 2, sink.session.MessageCount())
	assert.Equal(t, "first second", sink.session.GetLastAssistantMessageContent())
	assert.Equal(t, 1, initial.Initial[0].Session.MessageCount())
	assert.Equal(t, 2, reconnect.Initial[0].Session.MessageCount())
}

func TestObservationGapResetsCommittedTranscriptAndSeedsLiveTail(t *testing.T) {
	committed := session.New(session.WithID("s"))
	committed.AddMessage(session.NewAgentMessage("a", &chat.Message{Role: chat.MessageRoleAssistant, Content: "committed"}))
	closed := make(chan runtime.SessionEvent)
	close(closed)
	sink := &transcriptSink{}
	initial := obs(10, []runtime.SessionEvent{{Sequence: 11, Event: runtime.AgentChoice("a", "s", "stale")}}, closed)
	initial.Initial[0].Session = committed.Clone()
	result := projectObservation(t.Context(), sink, initial, nil)
	require.Error(t, result.err)

	gap := obs(20, []runtime.SessionEvent{{Gap: true}, {Sequence: 20, Event: runtime.AgentChoice("a", "s", "must not apply")}}, closed)
	gap.Initial[0].Session = committed.Clone()
	result = projectObservation(t.Context(), sink, gap, &result.cursor)
	require.True(t, result.gap)
	require.Equal(t, []uint64{10}, sink.resets)
	require.Equal(t, "stale", sink.live)

	message := session.NewAgentMessage("a", &chat.Message{Role: chat.MessageRoleAssistant, Content: "settled during gap"})
	committed.AddMessage(message)
	fresh := obs(20, []runtime.SessionEvent{
		{Sequence: 20, Event: &runtime.MessageAddedEvent{Message: message, SessionPosition: 1}},
		{Event: runtime.AgentChoice("a", "s", "fresh live ")},
	}, closed)
	fresh.Initial[0].Session = committed.Clone()
	fresh.Initial[0].TranscriptPosition = 2
	result = projectObservation(t.Context(), sink, fresh, nil)
	require.Error(t, result.err)
	assert.Equal(t, uint64(20), result.cursor)
	assert.Equal(t, []uint64{10, 20}, sink.resets)
	assert.Equal(t, []uint64{11, 0}, sink.applied)
	assert.Equal(t, "fresh live ", sink.live)
	assert.Equal(t, 2, sink.session.MessageCount())
	assert.Equal(t, "settled during gap", sink.session.GetLastAssistantMessageContent())
	assert.Equal(t, 1, initial.Initial[0].Session.MessageCount())
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
	require.ErrorAs(t, sink.errors[0], &treeErr)
	assert.Empty(t, sink.resets)
}

func TestCanonicalEventsRespectSnapshotReplayAndReattachBarriers(t *testing.T) {
	settled := func(sequence uint64) runtime.SessionEvent {
		return runtime.SessionEvent{
			SessionID: "owner", TurnID: "accepted", Sequence: sequence,
			Event: &runtime.TurnSettledEvent{SessionID: "owner", TurnID: "accepted", Outcome: runtime.TurnCompleted},
		}
	}
	created := runtime.SessionEvent{
		SessionID: "owner", TurnID: "accepted", Sequence: 9,
		Event: &runtime.SubagentCreatedEvent{SessionID: "owner", ParentSessionID: "owner", ChildSessionID: "child", NodeID: "node"},
	}
	live := make(chan runtime.SessionEvent, 3)
	live <- settled(7) // replay/live overlap
	live <- settled(8) // event published after the observation boundary
	live <- created
	close(live)
	sink := &recordingSink{}
	first := projectObservation(t.Context(), sink, obs(6, []runtime.SessionEvent{settled(5), settled(6), settled(7)}, live), nil)
	assert.Equal(t, []uint64{6}, sink.resets)
	assert.Equal(t, []uint64{7, 8, 9}, sink.applied)
	assert.Equal(t, uint64(9), first.cursor)
	reconnected := make(chan runtime.SessionEvent, 1)
	reconnected <- settled(10)
	close(reconnected)
	second := projectObservation(t.Context(), sink, obs(10, []runtime.SessionEvent{settled(8), created, settled(10)}, reconnected), &first.cursor)
	assert.Equal(t, []uint64{6}, sink.resets, "reattach preserves the prior projection rather than reseeding business events")
	assert.Equal(t, []uint64{7, 8, 9, 10}, sink.applied)
	assert.Equal(t, uint64(10), second.cursor)
	gap := make(chan runtime.SessionEvent, 1)
	gap <- runtime.SessionEvent{Gap: true}
	close(gap)
	result := projectObservation(t.Context(), sink, obs(10, nil, gap), &second.cursor)
	assert.True(t, result.gap)
	fresh := make(chan runtime.SessionEvent, 1)
	fresh <- settled(12)
	close(fresh)
	projectObservation(t.Context(), sink, obs(11, []runtime.SessionEvent{settled(11)}, fresh), nil)
	assert.Equal(t, []uint64{6, 11}, sink.resets)
	assert.Equal(t, []uint64{7, 8, 9, 10, 12}, sink.applied, "gap baseline is not recounted")
}

type observerFunc func(context.Context, runtime.ObserveOptions) (runtime.Observation, error)

func (f observerFunc) Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	return f(ctx, options)
}

type classifiedError bool

func (e classifiedError) Error() string   { return "classified observation failure" }
func (e classifiedError) Retryable() bool { return bool(e) }

func TestObservationOutageRecoveryResetsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	clock := time.Now()
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		if calls <= 12 {
			return runtime.Observation{}, errors.New("offline")
		}
		events := make(chan runtime.SessionEvent)
		close(events)
		if calls == 13 {
			// A healthy idle connection resets too, without replay progress.
			return obs(0, nil, events), nil
		}
		return obs(0, []runtime.SessionEvent{{Sequence: 1}}, events), nil
	})
	var attempts []int
	sink := &recordingSink{}
	a := &Attachment{done: make(chan struct{})}
	a.runWithRetry(ctx, observer, sink, observationRetryPolicy{
		now: func() time.Time {
			now := clock
			if calls == 13 {
				clock = clock.Add(10 * time.Second)
			}
			return now
		},
		wait: func(_ context.Context, attempt int) bool {
			attempts = append(attempts, attempt)
			return len(attempts) < 14
		},
	})
	assert.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 8, 8, 8, 0, 0}, attempts)
	assert.Equal(t, []uint64{1}, sink.applied)
	assert.Empty(t, sink.errors)
}

func TestObservationTerminalFailureDoesNotRetry(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "attach", true: "live"}[live], func(t *testing.T) {
			observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
				if !live {
					return runtime.Observation{}, classifiedError(false)
				}
				events := make(chan runtime.SessionEvent)
				errs := make(chan error, 1)
				errs <- classifiedError(false)
				close(events)
				close(errs)
				observation := obs(0, nil, events)
				observation.Errors = errs
				return observation, nil
			})
			sink := &recordingSink{}
			a := &Attachment{done: make(chan struct{})}
			a.runWithRetry(t.Context(), observer, sink, observationRetryPolicy{
				now:  time.Now,
				wait: func(context.Context, int) bool { t.Fatal("terminal failure retried"); return false },
			})
			require.Len(t, sink.errors, 1)
		})
	}
}

func TestObservationBackoffAndErrorDrainCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, waitRetry(ctx, 100000))
	ctx, cancel = context.WithCancel(t.Context())
	events := make(chan runtime.SessionEvent)
	close(events)
	observation := obs(0, nil, events)
	observation.Errors = make(chan error) // Broken provider must not trap Detach.
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) { return observation, nil })
	attachment, err := Attach(ctx, observer, &recordingSink{})
	require.NoError(t, err)
	cancel()
	done := make(chan struct{})
	go func() { attachment.Detach(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detach blocked on error drain")
	}
}

type approvalSink struct {
	recordingSink
	interactions []string
}

func (s *approvalSink) Reset(snapshot runtime.SessionSnapshot) {
	s.recordingSink.Reset(snapshot)
	s.interactions = nil
	for _, interaction := range snapshot.Interactions {
		s.interactions = append(s.interactions, interaction.InteractionID)
	}
}

func TestObservationGapResnapshotReplacesOutstandingApprovals(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	sink := &approvalSink{}
	first := obs(1, nil, events)
	first.Initial[0].Interactions = []runtime.InteractionSnapshot{{InteractionID: "old-approval"}}
	result := projectObservation(t.Context(), sink, first, nil)
	gap := obs(5, []runtime.SessionEvent{{Gap: true}}, events)
	result = projectObservation(t.Context(), sink, gap, &result.cursor)
	require.True(t, result.gap)
	fresh := obs(5, nil, events)
	fresh.Initial[0].Interactions = []runtime.InteractionSnapshot{{InteractionID: "new-approval"}}
	projectObservation(t.Context(), sink, fresh, nil)
	assert.Equal(t, []string{"new-approval"}, sink.interactions)
	assert.Equal(t, []uint64{1, 5}, sink.resets)
}

func TestAttachRemoteAuthorizationFailureIsTerminal(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	remote, err := runtime.NewClient(server.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(remote)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	sink := &recordingSink{}
	attachment, err := Attach(t.Context(), handle, sink)
	require.NoError(t, err)
	select {
	case <-attachment.done:
	case <-time.After(time.Second):
		attachment.Detach()
		t.Fatal("unauthorized attachment retried")
	}
	attachment.Detach()
	assert.Equal(t, int32(1), requests.Load())
	require.Len(t, sink.errors, 1)
	assert.ErrorContains(t, sink.errors[0], "401")
}

func TestObservationStartupIsFiniteWithoutProgress(t *testing.T) {
	clock := time.Now()
	calls := 0
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		return runtime.Observation{}, errors.New("offline")
	})
	sink := &recordingSink{}
	a := &Attachment{done: make(chan struct{})}
	a.runWithRetry(t.Context(), observer, sink, observationRetryPolicy{
		now:  func() time.Time { return clock },
		wait: func(context.Context, int) bool { clock = clock.Add(10 * time.Second); return true },
	})
	assert.Equal(t, 4, calls)
	require.Len(t, sink.errors, 1)
	assert.ErrorContains(t, sink.errors[0], "startup timed out")
}

func TestObservationHealthyProgressAllowsProlongedOutage(t *testing.T) {
	clock := time.Now()
	calls := 0
	events := make(chan runtime.SessionEvent)
	close(events)
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		if calls == 1 {
			return obs(0, []runtime.SessionEvent{{Sequence: 1}}, events), nil
		}
		if calls == 14 {
			return runtime.Observation{}, classifiedError(false)
		}
		return runtime.Observation{}, errors.New("temporary outage")
	})
	sink := &recordingSink{}
	a := &Attachment{done: make(chan struct{})}
	a.runWithRetry(t.Context(), observer, sink, observationRetryPolicy{
		now:  func() time.Time { return clock },
		wait: func(context.Context, int) bool { clock = clock.Add(time.Minute); return true },
	})
	assert.Equal(t, 14, calls)
	assert.Equal(t, []uint64{1}, sink.applied)
	require.Len(t, sink.errors, 1)
	assert.NotContains(t, sink.errors[0].Error(), "startup")
}

func TestObservationEpochChangeReplacesTranscriptAndInteractions(t *testing.T) {
	for _, nextCursor := range []uint64{2, 10} {
		t.Run(strconv.FormatUint(nextCursor, 10), func(t *testing.T) {
			events := make(chan runtime.SessionEvent)
			close(events)
			sink := &approvalSink{}
			first := obs(5, nil, events)
			first.Initial[0].Epoch = "old"
			first.Initial[0].Interactions = []runtime.InteractionSnapshot{{InteractionID: "stale"}}
			result := projectObservationWithEpoch(t.Context(), sink, first, nil, "", func() {})
			fresh := obs(nextCursor, []runtime.SessionEvent{{Epoch: "new", Event: runtime.AgentChoice("a", "s", "fresh tail")}}, events)
			fresh.Initial[0].Epoch = "new"
			fresh.Initial[0].Interactions = []runtime.InteractionSnapshot{{InteractionID: "fresh"}}
			fresh.Initial[0].PendingInputs = []runtime.PendingInput{{TurnID: "new-input"}}
			result = projectObservationWithEpoch(t.Context(), sink, fresh, &result.cursor, result.epoch, func() {})
			assert.Equal(t, []uint64{5, nextCursor}, sink.resets)
			assert.Equal(t, []string{"fresh"}, sink.interactions)
			assert.Equal(t, []string{"new-input"}, sink.pending)
			assert.Equal(t, []uint64{0}, sink.applied)
			assert.Equal(t, nextCursor, result.cursor)
			assert.Equal(t, "new", result.epoch)
		})
	}
}

func TestAttachmentReconnectCarriesEpochAndResetsAfterRestart(t *testing.T) {
	calls := 0
	observer := observerFunc(func(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		if calls > 2 {
			return runtime.Observation{}, classifiedError(false)
		}
		events := make(chan runtime.SessionEvent)
		close(events)
		observation := obs(5, nil, events)
		observation.Initial[0].Epoch = "old"
		if calls == 2 {
			assert.Equal(t, "old", options.SinceEpoch)
			require.NotNil(t, options.Since)
			assert.Equal(t, uint64(5), *options.Since)
			observation.Initial[0].Epoch = "new"
			observation.Initial[0].Cursor = 10
		}
		return observation, nil
	})
	sink := &recordingSink{}
	a := &Attachment{done: make(chan struct{})}
	a.runWithRetry(t.Context(), observer, sink, observationRetryPolicy{now: time.Now, wait: func(context.Context, int) bool { return true }})
	assert.Equal(t, []uint64{5, 10}, sink.resets)
}

func TestObservationSameEpochReconnectPreservesStreamingTail(t *testing.T) {
	events := make(chan runtime.SessionEvent)
	close(events)
	sink := &transcriptSink{}
	first := obs(5, []runtime.SessionEvent{{Epoch: "process", Sequence: 6, Event: runtime.AgentChoice("a", "s", "first ")}}, events)
	first.Initial[0].Epoch = "process"
	first.Initial[0].Session = session.New(session.WithID("s"))
	result := projectObservationWithEpoch(t.Context(), sink, first, nil, "", func() {})
	second := obs(7, []runtime.SessionEvent{{Epoch: "process", Sequence: 7, Event: runtime.AgentChoice("a", "s", "second")}}, events)
	second.Initial[0].Epoch = "process"
	second.Initial[0].Session = first.Initial[0].Session.Clone()
	result = projectObservationWithEpoch(t.Context(), sink, second, &result.cursor, result.epoch, func() {})
	assert.Equal(t, []uint64{5}, sink.resets)
	assert.Equal(t, "first second", sink.live)
	assert.Equal(t, uint64(7), result.cursor)
}

type connectionSink struct {
	recordingSink

	transitions []bool
	failures    []error
}

func (s *connectionSink) OnConnectionState(connected bool, err error) {
	s.transitions = append(s.transitions, connected)
	s.failures = append(s.failures, err)
}

func TestAttachmentReportsConnectionStateAcrossReconnect(t *testing.T) {
	offline := errors.New("offline")
	calls := 0
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		calls++
		switch calls {
		case 1, 3:
			events := make(chan runtime.SessionEvent)
			close(events)
			return obs(0, nil, events), nil
		case 2:
			return runtime.Observation{}, offline
		default:
			return runtime.Observation{}, classifiedError(false)
		}
	})
	sink := &connectionSink{}
	waits := 0
	attachment := &Attachment{done: make(chan struct{})}
	attachment.runWithRetry(t.Context(), observer, sink, observationRetryPolicy{
		now: time.Now,
		wait: func(context.Context, int) bool {
			waits++
			require.Len(t, sink.transitions, []int{2, 3, 5}[waits-1], "disconnect callback precedes backoff")
			assert.False(t, sink.transitions[len(sink.transitions)-1])
			return true
		},
	})
	assert.Equal(t, []bool{true, false, false, true, false}, sink.transitions)
	require.Len(t, sink.failures, 5)
	require.NoError(t, sink.failures[0])
	require.ErrorContains(t, sink.failures[1], "observation stream closed")
	require.ErrorIs(t, sink.failures[2], offline)
	require.NoError(t, sink.failures[3])
	require.ErrorContains(t, sink.failures[4], "observation stream closed")
	assert.Equal(t, 3, waits)
	require.Len(t, sink.errors, 1)
}

func TestAttachmentReportsGapBeforeReconnectBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sink := &connectionSink{}
	observer := observerFunc(func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
		return obs(0, []runtime.SessionEvent{{Gap: true}}, nil), nil
	})
	attachment := &Attachment{done: make(chan struct{})}
	attachment.runWithRetry(ctx, observer, sink, observationRetryPolicy{
		now: time.Now,
		wait: func(context.Context, int) bool {
			assert.Equal(t, []bool{true, false}, sink.transitions)
			require.NoError(t, sink.failures[1])
			cancel()
			return false
		},
	})
	assert.Empty(t, sink.errors)
}
