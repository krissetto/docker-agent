package server

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type interactionStream struct {
	responses []chat.MessageStreamResponse
	i         int
}

func (s *interactionStream) Recv() (chat.MessageStreamResponse, error) {
	if s.i >= len(s.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}
func (*interactionStream) Close() {}

type interactionProvider struct {
	streams []chat.MessageStream
	i       int
}

func (*interactionProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/http-interaction")
}

func (p *interactionProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	s := p.streams[p.i]
	p.i++
	return s, nil
}
func (*interactionProvider) BaseConfig() base.Config { return base.Config{} }

type interactionToolSet struct{ tool tools.Tool }

func (s interactionToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{s.tool}, nil
}

func toolCallStream() chat.MessageStream {
	return &interactionStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "confirm", Type: "function", Function: tools.FunctionCall{Name: "danger", Arguments: "{}"}}}}}}}, {Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}}}}
}

func stopStream() chat.MessageStream {
	return &interactionStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "done"}}}}, {Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}}}}
}

func realInteractionHTTP(t *testing.T, maximum int) (*runtime.SessionTransport, func()) {
	t.Helper()
	streams := []chat.MessageStream{toolCallStream(), stopStream(), toolCallStream(), stopStream()}
	prov := &interactionProvider{streams: streams}
	tool := tools.Tool{Name: "danger", Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess("ok"), nil
	}}
	agt := agent.New("root", "prompt", agent.WithModel(prov), agent.WithToolSets(interactionToolSet{tool}), agent.WithMaxIterations(maximum))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agt)), runtime.WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(t.Context(), nil, store, 0, nil, WithSessionRuntime(owner.Runtime()))
	httpSrv := httptest.NewServer(NewWithManager(sm, "").e)
	client, err := runtime.NewClient(httpSrv.URL, runtime.WithHTTPClient(httpSrv.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	return transport, func() { httpSrv.Close(); _ = owner.Shutdown(t.Context()) }
}

func runHTTPInteraction(t *testing.T, maximum int, kind runtime.InteractionKind) {
	t.Helper()
	transport, closeFn := realInteractionHTTP(t, maximum)
	defer closeFn()
	handle, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	_, err = handle.Submit(t.Context(), runtime.TurnInput{Content: "run"})
	require.NoError(t, err)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for expected session interaction")
		case e, ok := <-obs.Events:
			if !ok {
				t.Fatal("session event stream closed before expected interaction")
			}
			if e.InteractionID == "" {
				continue
			}
			if _, ok := e.Event.(*runtime.ToolCallConfirmationEvent); ok {
				require.NoError(t, handle.Respond(t.Context(), runtime.InteractionResponse{InteractionID: e.InteractionID, Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeApprove()}))
				if kind == runtime.InteractionConfirmation {
					return
				}
				continue
			}
			if _, ok := e.Event.(*runtime.MaxIterationsReachedEvent); ok && kind == runtime.InteractionMaxIterations {
				require.NoError(t, handle.Respond(t.Context(), runtime.InteractionResponse{InteractionID: e.InteractionID, Kind: kind, Resume: runtime.ResumeReject("stop")}))
				return
			}
		}
	}
}

func TestCanonicalHTTPRealToolConfirmationResponseRoundTrip(t *testing.T) {
	runHTTPInteraction(t, 10, runtime.InteractionConfirmation)
}

func TestCanonicalHTTPRealMaxIterationsResponseRoundTrip(t *testing.T) {
	runHTTPInteraction(t, 1, runtime.InteractionMaxIterations)
}
