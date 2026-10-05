package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type pausedInteractionObserver struct {
	entered chan struct{}
	release chan struct{}
	kind    InteractionKind
}

func (*pausedInteractionObserver) OnRunStart(context.Context, *session.Session) {}
func (o *pausedInteractionObserver) OnEvent(ctx context.Context, _ *session.Session, e Event) {
	if interactionKindOf(e) == o.kind {
		close(o.entered)
		select {
		case <-o.release:
		case <-ctx.Done():
		}
	}
}

type interactionToolProvider struct{ mockProvider }

func (p *interactionToolProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	for _, m := range messages {
		if m.Role == chat.MessageRoleTool {
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
	}
	return newStreamBuilder().AddToolCallName("probe", "probe").AddToolCallArguments("probe", "{}").AddToolCallStopWithUsage(1, 1).Build(), nil
}

func TestInteractionOpeningPrecedesSnapshotResponse(t *testing.T) {
	for _, kind := range []InteractionKind{InteractionConfirmation, InteractionMaxIterations} {
		t.Run(string(kind), func(t *testing.T) {
			observer := &pausedInteractionObserver{entered: make(chan struct{}), release: make(chan struct{}), kind: kind}
			release := sync.OnceFunc(func() { close(observer.release) })
			defer release()
			tool := tools.Tool{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				return tools.ResultSuccess("done"), nil
			}}
			prov := &interactionToolProvider{mockProvider: mockProvider{id: "test/mock-model"}}
			maxIterations := 0
			if kind == InteractionMaxIterations {
				maxIterations = 1
			}
			tm := team.New(team.WithAgents(agent.New("root", "Use probe.", agent.WithModel(prov), agent.WithMaxIterations(maxIterations), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{tool}, nil)))))
			rt, err := NewLocalRuntime(t.Context(), tm, WithEventObserver(observer), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)
			t.Cleanup(func() { release(); require.NoError(t, rt.Close()) })
			handle, err := rt.CreateSession(t.Context(), session.New(session.WithID("ordering"), session.WithToolsApproved(kind == InteractionMaxIterations)), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			original, err := handle.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			defer original.Cancel()
			submission, err := handle.Submit(t.Context(), TurnInput{Content: "use tool"})
			require.NoError(t, err)
			select {
			case <-observer.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("confirmation observer did not block")
			}
			attached, err := handle.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			defer attached.Cancel()
			require.Len(t, attached.Primary().Interactions, 1)
			interaction := attached.Primary().Interactions[0]
			require.NotNil(t, interaction.Event)
			response := ResumeApprove()
			if kind == InteractionMaxIterations {
				response = ResumeReject("stop")
			}
			require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: interaction.InteractionID, Kind: interaction.Kind, Resume: response}))
			release()
			var order []string
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			for {
				select {
				case envelope, ok := <-original.Events:
					require.True(t, ok)
					switch envelope.Event.(type) {
					case *InteractionResolvedEvent:
						order = append(order, "resolved")
					case *ToolCallConfirmationEvent:
						if kind == InteractionConfirmation {
							order = append(order, "opened")
						}
					case *MaxIterationsReachedEvent:
						if kind == InteractionMaxIterations {
							order = append(order, "opened")
						}
					case *StreamStoppedEvent:
						require.Equal(t, []string{"opened", "resolved"}, order)
						require.NoError(t, handle.AwaitTurn(t.Context(), submission.TurnID))
						final, err := handle.Observe(t.Context(), ObserveOptions{})
						require.NoError(t, err)
						t.Cleanup(final.Cancel)
						require.Empty(t, final.Primary().Interactions)
						zero := uint64(0)
						replayed, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero, SinceEpoch: original.Primary().Epoch})
						require.NoError(t, err)
						t.Cleanup(replayed.Cancel)
						var replayOrder []string
						for _, envelope := range replayed.Replay {
							if envelope.InteractionID != interaction.InteractionID {
								continue
							}
							if _, ok := envelope.Event.(*InteractionResolvedEvent); ok {
								replayOrder = append(replayOrder, "resolved")
							} else if interactionKindOf(envelope.Event) == kind {
								replayOrder = append(replayOrder, "opened")
							}
						}
						require.Equal(t, []string{"opened", "resolved"}, replayOrder)
						return
					}
				case <-timer.C:
					t.Fatal("turn did not settle")
				}
			}
		})
	}
}

func TestElicitationSubcontextCancellationPublishesResolution(t *testing.T) {
	childCancel := make(chan context.CancelFunc, 1)
	returned := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	tool := tools.Tool{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		childCancel <- cancel
		_, err := tools.ScopedElicitationHandler(child, &mcp.ElicitParams{Message: "question", Mode: "form"})
		if err == nil {
			return nil, errors.New("expected child cancellation")
		}
		close(returned)
		<-release
		return tools.ResultSuccess("question abandoned; other tool work done"), nil
	}}
	prov := &interactionToolProvider{mockProvider: mockProvider{id: "test/mock-model"}}
	tm := team.New(team.WithAgents(agent.New("root", "Use probe.", agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{tool}, nil)))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { releaseOnce(); require.NoError(t, rt.Close()) })
	handle, err := rt.CreateSession(t.Context(), session.New(session.WithID("review-elicit"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	_, err = handle.Submit(t.Context(), TurnInput{Content: "use tool"})
	require.NoError(t, err)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var token string
	for token == "" {
		select {
		case e := <-obs.Events:
			if _, ok := e.Event.(*ElicitationRequestEvent); ok {
				token = e.InteractionID
			}
		case <-timer.C:
			t.Fatal("no elicitation")
		}
	}
	(<-childCancel)()
	<-returned
	fresh, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	require.Empty(t, fresh.Primary().Interactions)
	require.Equal(t, SessionStateRunning, fresh.Primary().Status.State)
	fresh.Cancel()
	releaseOnce()
	resolutions := 0
	for {
		select {
		case e := <-obs.Events:
			if resolved, ok := e.Event.(*InteractionResolvedEvent); ok && resolved.InteractionID == token {
				resolutions++
				require.Equal(t, InteractionCanceled, resolved.Reason)
			}
			if _, ok := e.Event.(*StreamStoppedEvent); ok {
				require.Equal(t, 1, resolutions)
				require.Error(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: token, Kind: InteractionElicitation, ElicitationID: token}))
				return
			}
		case <-timer.C:
			t.Fatal("no terminal")
		}
	}
}
