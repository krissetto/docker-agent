package runtime

import (
	"context"
	"io"
	"sync"
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
	started chan struct{}
	release chan struct{}
	once    sync.Once
	close   sync.Once
}

func (s *cancelBarrierStream) Recv() (chat.MessageStreamResponse, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return chat.MessageStreamResponse{}, io.EOF
}
func (s *cancelBarrierStream) Close() { s.close.Do(func() { close(s.release) }) }

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
	assert.Equal(t, "second", sess.GetLastAssistantMessageContent())
}
