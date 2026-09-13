package client

import (
	"context"
	"errors"
	"sync"
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

func TestObservationTerminalErrorsReconnectWithCursorAndExhaust(t *testing.T) {
	a := &sessionStub{observations: make(chan runtime.Observation, 4)}
	for i := range 4 {
		events := make(chan runtime.SessionEvent)
		errs := make(chan error, 1)
		errs <- errors.New("dropped")
		close(errs)
		close(events)
		var replay []runtime.SessionEvent
		if i > 0 {
			replay = []runtime.SessionEvent{{Sequence: uint64(i + 1)}}
		}
		a.observations <- runtime.Observation{Initial: []runtime.SessionSnapshot{{Cursor: uint64(i + 1)}}, Replay: replay, Events: events, Errors: errs, Cancel: func() {}}
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
	defer attachment.Detach()
	select {
	case <-attachment.done:
	case <-time.After(2 * time.Second):
		t.Fatal("observation retries did not finish")
	}
	require.Len(t, a.since, 4)
	assert.Nil(t, a.since[0])
	for _, since := range a.since[1:] {
		require.NotNil(t, since)
		assert.Zero(t, *since)
	}
	assert.Equal(t, []uint64{0}, sink.resets)
	require.Len(t, sink.errors, 1)
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
