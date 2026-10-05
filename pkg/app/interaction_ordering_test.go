package app

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type projectionConfirmationObserver struct{ entered, release chan struct{} }

func (*projectionConfirmationObserver) OnRunStart(context.Context, *session.Session) {}
func (o *projectionConfirmationObserver) OnEvent(ctx context.Context, _ *session.Session, event runtime.Event) {
	if _, ok := event.(*runtime.ToolCallConfirmationEvent); ok {
		close(o.entered)
		select {
		case <-o.release:
		case <-ctx.Done():
		}
	}
}

type projectionToolSet struct{}

func (projectionToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess("done"), nil
	}}}, nil
}

type projectionToolProvider struct{ stubProvider }

func (projectionToolProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	for _, message := range messages {
		if message.Role == chat.MessageRoleTool {
			return &replyStream{}, nil
		}
	}
	return &projectionToolStream{}, nil
}

type projectionToolStream struct{ index int }

func (s *projectionToolStream) Recv() (chat.MessageStreamResponse, error) {
	s.index++
	switch s.index {
	case 1:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "probe", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}}}}}}, nil
	case 2:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}
func (*projectionToolStream) Close() {}

func TestCanonicalConfirmationSnapshotResponseDoesNotReopenProjection(t *testing.T) {
	observer := &projectionConfirmationObserver{entered: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(observer.release) })
	defer release()
	tm := team.New(team.WithAgents(agent.New("root", "Use probe.", agent.WithModel(projectionToolProvider{}), agent.WithToolSets(projectionToolSet{}))))
	rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithEventObserver(observer), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { release(); require.NoError(t, rt.Close()) })
	h, err := rt.CreateSession(t.Context(), session.New(session.WithID("s")), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	a := newMetadataTestApp(t)
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "s"}
	sink.Reset(obs.Primary())
	submission, err := h.Submit(t.Context(), runtime.TurnInput{Content: "use tool"})
	require.NoError(t, err)
	select {
	case <-observer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not pause")
	}
	fresh, err := h.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer fresh.Cancel()
	require.Len(t, fresh.Primary().Interactions, 1)
	prompt := fresh.Primary().Interactions[0]
	require.NoError(t, h.Respond(t.Context(), runtime.InteractionResponse{InteractionID: prompt.InteractionID, Kind: prompt.Kind, Resume: runtime.ResumeApprove()}))
	release()
	key := InteractionKey{SessionID: "s", InteractionID: prompt.InteractionID}
	var order []string
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case envelope := <-obs.Events:
			sink.Apply(envelope)
			switch envelope.Event.(type) {
			case *runtime.ToolCallConfirmationEvent:
				order = append(order, "opened")
				require.True(t, a.Presentation().HasInteraction(key))
			case *runtime.InteractionResolvedEvent:
				order = append(order, "resolved")
				require.False(t, a.Presentation().HasInteraction(key))
			case *runtime.StreamStoppedEvent:
				require.Equal(t, []string{"opened", "resolved"}, order)
				require.False(t, a.Presentation().HasInteraction(key))
				require.NoError(t, h.AwaitTurn(t.Context(), submission.TurnID))
				return
			}
		case <-timer.C:
			t.Fatal("turn did not settle")
		}
	}
}
