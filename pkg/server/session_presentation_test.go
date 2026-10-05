package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type presentationTitleProvider struct {
	entered chan struct{}
	release chan struct{}
	failure bool
	calls   atomic.Int32
}

func (*presentationTitleProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/title") }
func (*presentationTitleProvider) BaseConfig() base.Config { return base.Config{} }
func (*presentationTitleProvider) MaxTokens() int          { return 128 }
func (p *presentationTitleProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.calls.Add(1)
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.failure {
		return nil, errors.New("offline title")
	}
	return &interactionStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "HTTP title"}}}}}}, nil
}

func TestCanonicalHTTPPreturnPresentationAndTitleResetTerminal(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "fallback"}[failure], func(t *testing.T) {
			p := &presentationTitleProvider{entered: make(chan struct{}), release: make(chan struct{}), failure: failure}
			srv, _ := newCanonicalLocalServer(t, session.NewInMemorySessionStore(), agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithTitleModel(p)))
			server := httptest.NewServer(srv.e)
			defer server.Close()
			client, err := runtime.NewClient(server.URL, runtime.WithHTTPClient(server.Client()))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client)
			require.NoError(t, err)
			h, err := transport.CreateSession(t.Context(), session.New(), runtime.SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			before, err := h.Observe(t.Context(), runtime.ObserveOptions{})
			require.NoError(t, err)
			defer before.Cancel()
			require.Len(t, before.Primary().Presentation, 2)
			require.Equal(t, "root", before.Primary().Presentation[0].(*runtime.AgentInfoEvent).AgentName)
			require.Equal(t, "test/http", before.Primary().Presentation[0].(*runtime.AgentInfoEvent).Model)
			require.Zero(t, p.calls.Load())
			submitted, err := h.Submit(t.Context(), runtime.TurnInput{Content: "first prompt", GenerateTitle: true})
			require.NoError(t, err)
			<-p.entered
			active, err := h.Observe(t.Context(), runtime.ObserveOptions{})
			require.NoError(t, err)
			require.Equal(t, "started", active.Primary().TitleStatus)
			active.Cancel()
			close(p.release)
			require.NoError(t, h.AwaitTurn(t.Context(), submitted.TurnID))
			deadline := time.After(5 * time.Second)
			for {
				select {
				case event := <-before.Events:
					if title, ok := event.Event.(*runtime.SessionTitleEvent); ok && title.Status == "completed" {
						require.NotEmpty(t, title.Title)
						after, err := h.Observe(t.Context(), runtime.ObserveOptions{})
						require.NoError(t, err)
						require.Equal(t, "completed", after.Primary().TitleStatus)
						require.Equal(t, title.Title, after.Primary().Session.TitleSnapshot())
						after.Cancel()
						return
					}
				case <-deadline:
					t.Fatal("title terminal event missing")
				}
			}
		})
	}
}
