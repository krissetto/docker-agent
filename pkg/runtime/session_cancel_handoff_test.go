package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type cancelHandoffProvider struct {
	mu      sync.Mutex
	streams []chat.MessageStream
	calls   int
}

func (p *cancelHandoffProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/cancel-handoff")
}
func (p *cancelHandoffProvider) BaseConfig() base.Config { return base.Config{} }
func (p *cancelHandoffProvider) MaxTokens() int          { return 0 }
func (p *cancelHandoffProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

func (p *cancelHandoffProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type cancelBarrierStream struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	close       sync.Once
	releaseOnce sync.Once
}

func (s *cancelBarrierStream) Recv() (chat.MessageStreamResponse, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return chat.MessageStreamResponse{}, io.EOF
}
func (s *cancelBarrierStream) unblock() { s.releaseOnce.Do(func() { close(s.release) }) }
func (s *cancelBarrierStream) Close()   { s.close.Do(s.unblock) }

func TestCancelImmediateSubmitPromotesRequestExactlyOnce(t *testing.T) {
	firstStarted := make(chan struct{})
	provider := &cancelHandoffProvider{streams: []chat.MessageStream{
		&cancelBarrierStream{started: firstStarted, release: make(chan struct{})},
		newStreamBuilder().AddContent("second").AddStopWithUsage(1, 1).Build(),
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	sess := session.New(session.WithID("cancel-handoff"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 128})
	require.NoError(t, err)
	defer obs.Cancel()

	first, err := handle.Submit(t.Context(), TurnInput{Content: "first"})
	require.NoError(t, err)
	<-firstStarted
	wake := handle.(interface {
		Cancel(ctx context.Context, turnID string) (CancelResult, error)
	})
	_, err = wake.Cancel(t.Context(), first.TurnID)
	require.NoError(t, err)
	second, err := handle.Submit(t.Context(), TurnInput{Content: "second"})
	require.NoError(t, err)

	var firstStops, secondAccepted, secondPromoted, secondStarts, secondStops int
	contentsByTurn := map[string][]string{}
	deadline := time.After(5 * time.Second)
	for secondStops == 0 {
		select {
		case envelope := <-obs.Events:
			switch event := envelope.Event.(type) {
			case *PendingUserMessagePromotedEvent:
				contentsByTurn[envelope.TurnID] = append(contentsByTurn[envelope.TurnID], event.Message)
				if envelope.TurnID == second.TurnID {
					secondPromoted++
				}
			case *PendingUserMessageAcceptedEvent:
				if envelope.TurnID == second.TurnID {
					secondAccepted++
				}
			}
			switch envelope.TurnID {
			case first.TurnID:
				if _, ok := envelope.Event.(*StreamStoppedEvent); ok {
					firstStops++
				}
			case second.TurnID:
				switch envelope.Event.(type) {
				case *StreamStartedEvent:
					secondStarts++
				case *UserMessageEvent:
				case *StreamStoppedEvent:
					secondStops++
				}
			}
		case <-deadline:
			t.Fatal("second accepted request did not finish")
		}
	}
	assert.Equal(t, 1, firstStops)
	assert.Equal(t, 1, secondAccepted)
	assert.Equal(t, 1, secondPromoted)
	assert.Equal(t, 1, secondStarts)
	assert.Equal(t, []string{"first"}, contentsByTurn[first.TurnID])
	assert.Equal(t, []string{"second"}, contentsByTurn[second.TurnID])
	assert.Equal(t, 1, secondStops)
	assert.Equal(t, "second", sessionHandleSnapshot(t, handle).GetLastAssistantMessageContent())
}

func TestCanceledSuccessorAdmissionFailureDrainsAcceptedFIFO(t *testing.T) {
	firstStarted := make(chan struct{})
	provider := &cancelHandoffProvider{streams: []chat.MessageStream{
		&cancelBarrierStream{started: firstStarted, release: make(chan struct{})},
		newStreamBuilder().AddContent("second").AddStopWithUsage(1, 1).Build(),
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	sess := session.New(session.WithID("cancel-successor-admission"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 128})
	require.NoError(t, err)
	defer obs.Cancel()

	first, err := handle.Submit(t.Context(), TurnInput{Content: "first"})
	require.NoError(t, err)
	<-firstStarted
	second, err := handle.Submit(t.Context(), TurnInput{Content: "second"})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.compactReserved = true
		return nil
	}))
	result, err := handle.Cancel(t.Context(), first.TurnID)
	require.NoError(t, err)
	require.Equal(t, CancelAccepted, result.Outcome)

	require.Eventually(t, func() bool {
		settled := false
		for _, event := range canonicalSettlements(canonicalReplay(driver)) {
			if event.Event.(*TurnSettledEvent).TurnID == first.TurnID {
				settled = true
			}
		}
		if !settled {
			return false
		}
		var starting, queued bool
		_ = driver.ownerCall(context.WithoutCancel(t.Context()), func() error {
			starting = driver.starting()
			queued = len(driver.pending) == 1 && driver.pending[0].RequestID == second.TurnID
			return nil
		})
		return !starting && queued
	}, 5*time.Second, time.Millisecond, "failed successor admission must leave the immutable FIFO available to retry")

	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.compactReserved = false
		return nil
	}))
	driver.r.sessionDrivers.signalWork()
	coordinationAwait(t, handle, second.TurnID)
	assert.Equal(t, 2, provider.callCount())
	assert.Equal(t, "second", sessionHandleSnapshot(t, handle).GetLastAssistantMessageContent())
	settled := canonicalSettlements(canonicalReplay(driver))
	require.Len(t, settled, 2)
	assert.Equal(t, TurnCanceled, settled[0].Event.(*TurnSettledEvent).Outcome)
	assert.Equal(t, second.TurnID, settled[1].Event.(*TurnSettledEvent).TurnID)
	assert.Equal(t, TurnCompleted, settled[1].Event.(*TurnSettledEvent).Outcome)
}

type transientSuccessorPromotionStore struct {
	session.Store
	failures atomic.Int64
}

func (s *transientSuccessorPromotionStore) PromotePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	if s.failures.Add(-1) >= 0 {
		return &session.TemporaryError{Err: errors.New("transient successor promotion")}
	}
	return s.Store.PromotePendingUserMessage(ctx, sessionID, turnID)
}

func TestSuccessorPromotionFailureClosesSettlementAndPreservesNewStart(t *testing.T) {
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	secondStream := &cancelBarrierStream{started: secondStarted, release: make(chan struct{})}
	store := &transientSuccessorPromotionStore{Store: coordinationSQLite(t)}
	provider := &cancelHandoffProvider{streams: []chat.MessageStream{
		&cancelBarrierStream{started: firstStarted, release: make(chan struct{})},
		secondStream,
		newStreamBuilder().AddContent("third").AddStopWithUsage(1, 1).Build(),
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))), WithSessionStore(store), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	sess := session.New(session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := handle.Submit(t.Context(), TurnInput{Content: "first"})
	require.NoError(t, err)
	<-firstStarted
	second, err := handle.Submit(t.Context(), TurnInput{Content: "second"})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver

	thirdResult := make(chan struct {
		submission Submission
		cancel     CancelResult
		err        error
	}, 1)
	var submitted atomic.Bool
	unregister := driver.OnSettled(func() {
		if !submitted.CompareAndSwap(false, true) {
			return
		}
		go func() {
			submission, submitErr := handle.Submit(t.Context(), TurnInput{Content: "third"})
			thirdResult <- struct {
				submission Submission
				cancel     CancelResult
				err        error
			}{submission: submission, err: submitErr}
		}()
	})
	defer unregister()
	store.failures.Store(1)
	result, err := handle.Cancel(t.Context(), first.TurnID)
	require.NoError(t, err)
	require.Equal(t, CancelAccepted, result.Outcome)
	coordinationWait(t, secondStarted)

	select {
	case outcome := <-thirdResult:
		require.NoError(t, outcome.err)
		third := outcome.submission
		result, cancelErr := handle.Cancel(t.Context(), second.TurnID)
		require.NoError(t, cancelErr)
		require.Equal(t, CancelAccepted, result.Outcome, "new start must remain cancellable after the old successor failure")
		coordinationAwait(t, handle, second.TurnID)
		coordinationAwait(t, handle, third.TurnID)
		assert.Equal(t, 3, provider.callCount())
		settled := canonicalSettlements(canonicalReplay(driver))
		require.Len(t, settled, 3)
		assert.Equal(t, first.TurnID, settled[0].Event.(*TurnSettledEvent).TurnID)
		assert.Equal(t, TurnCanceled, settled[0].Event.(*TurnSettledEvent).Outcome)
		assert.Equal(t, second.TurnID, settled[1].Event.(*TurnSettledEvent).TurnID)
		assert.Equal(t, TurnCanceled, settled[1].Event.(*TurnSettledEvent).Outcome)
		assert.Equal(t, third.TurnID, settled[2].Event.(*TurnSettledEvent).TurnID)
		assert.Equal(t, TurnCompleted, settled[2].Event.(*TurnSettledEvent).Outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("successor promotion failure did not release settlement waiters and trigger the newer accepted start")
	}
}
