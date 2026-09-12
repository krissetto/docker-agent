package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type normalHandoffStream struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	index   int
}

func (s *normalHandoffStream) Recv() (chat.MessageStreamResponse, error) {
	switch s.index {
	case 0:
		s.index++
		s.once.Do(func() { close(s.started) })
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: "first"}}}}, nil
	case 1:
		s.index++
		<-s.release
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}

func (*normalHandoffStream) Close() {}

type handoffErrorStream struct{}

func (*handoffErrorStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, errors.New("turn failed")
}
func (*handoffErrorStream) Close() {}

func TestErrorCompletionAutomaticallyPromotesQueuedSuccessor(t *testing.T) {
	provider := &cancelHandoffProvider{streams: []chat.MessageStream{
		&handoffErrorStream{},
		newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build(),
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithID("error-handoff"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = handle.Submit(t.Context(), TurnInput{Content: "A"})
	require.NoError(t, err)
	_, err = handle.Submit(t.Context(), TurnInput{Content: "B"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == SessionStateSettled && status.Pending == 0
	}, 5*time.Second, time.Millisecond)
	assert.Equal(t, 2, provider.callCount())
	assert.Equal(t, "recovered", sess.GetLastAssistantMessageContent())
}

// A UI owns the context passed to Submit and cancels it when that turn's stop
// reaches the model. Successors are session-owned work: their execution must not
// inherit that caller context, even when the predecessor completed normally.
func TestNormalCompletionHandoffDetachesCallerContextAndDrainsFIFO(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	secondRelease := make(chan struct{})
	provider := &cancelHandoffProvider{streams: []chat.MessageStream{
		&normalHandoffStream{started: started, release: release},
		&normalHandoffStream{started: make(chan struct{}), release: secondRelease},
		newStreamBuilder().AddContent("third").AddStopWithUsage(1, 1).Build(),
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })

	sess := session.New(session.WithID("normal-handoff"), session.WithAgentName("root"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 256})
	require.NoError(t, err)
	defer obs.Cancel()

	callerCtx, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	first, err := handle.Submit(callerCtx, TurnInput{Content: "A"})
	require.NoError(t, err)
	<-started
	close(release)

	turns := []string{first.TurnID}
	secondTurn := ""
	thirdTurn := ""
	starts := map[string]int{}
	stops := map[string]int{}
	promotions := map[string]int{}
	order := make([]string, 0, 3)
	deadline := time.After(5 * time.Second)
	for thirdTurn == "" || provider.callCount() < 3 {
		select {
		case envelope := <-obs.Events:
			switch envelope.Event.(type) {
			case *StreamStartedEvent:
				starts[envelope.TurnID]++
				order = append(order, envelope.TurnID)
				if envelope.TurnID == secondTurn {
					third, submitErr := handle.Submit(t.Context(), TurnInput{Content: "C"})
					require.NoError(t, submitErr)
					thirdTurn = third.TurnID
					turns = append(turns, third.TurnID)
					close(secondRelease)
				}
			case *PendingUserMessagePromotedEvent:
				promotions[envelope.TurnID]++
			case *StreamStoppedEvent:
				stops[envelope.TurnID]++
				if envelope.TurnID == first.TurnID {
					second, submitErr := handle.Submit(t.Context(), TurnInput{Content: "B"})
					require.NoError(t, submitErr)
					secondTurn = second.TurnID
					turns = append(turns, second.TurnID)
					cancelCaller() // no key/input/nudge starts B
				}
			}
		case <-deadline:
			t.Fatalf("FIFO did not drain: calls=%d starts=%v stops=%v", provider.callCount(), starts, stops)
		}
	}

	assert.GreaterOrEqual(t, len(order), 2)
	assert.Equal(t, turns[:2], order[:2], "observed starts preserve FIFO")
	assert.Equal(t, 1, starts[first.TurnID])
	assert.Equal(t, 1, starts[turns[1]])
	assert.Equal(t, 1, stops[first.TurnID])
	assert.Equal(t, 1, promotions[turns[1]])
	assert.LessOrEqual(t, promotions[turns[2]], 1)
	assert.Equal(t, 3, provider.callCount())
	require.Eventually(t, func() bool {
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == SessionStateSettled && status.Pending == 0
	}, 5*time.Second, time.Millisecond)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateSettled, status.State)
	assert.Zero(t, status.Pending)
	assert.Empty(t, status.LastError)
	assert.Equal(t, "third", sess.GetLastAssistantMessageContent())
	var users []string
	for _, item := range sess.GetAllMessages() {
		if item.Message.Role == chat.MessageRoleUser {
			users = append(users, item.Message.Content)
			assert.False(t, item.Pending)
		}
	}
	assert.Equal(t, []string{"A", "B", "C"}, users)
}
