package tui_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

type tuiCancelProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (p *tuiCancelProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/tui-cancel") }
func (p *tuiCancelProvider) BaseConfig() base.Config { return base.Config{} }
func (p *tuiCancelProvider) MaxTokens() int          { return 0 }
func (p *tuiCancelProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		return &tuiCancelStream{done: ctx.Done()}, nil
	}
	return &tuiAnswerStream{}, nil
}

type tuiCancelStream struct{ done <-chan struct{} }

func (s *tuiCancelStream) Recv() (chat.MessageStreamResponse, error) {
	<-s.done
	return chat.MessageStreamResponse{}, context.Canceled
}
func (*tuiCancelStream) Close() {}

type tuiAnswerStream struct{ step int }

func (s *tuiAnswerStream) Recv() (chat.MessageStreamResponse, error) {
	switch s.step {
	case 0:
		s.step++
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "second answer"}}}}, nil
	case 1:
		s.step++
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}
func (*tuiAnswerStream) Close() {}

func TestActualTUICancelImmediateSendShowsAcceptedTurnAndSettles(t *testing.T) {
	p := &tuiCancelProvider{started: make(chan struct{})}
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(p)))))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { _ = owner.Shutdown(context.WithoutCancel(t.Context())) })
	sess := session.New(session.WithID("cancel-ui"), session.WithAgentName("root"))
	a := app.New(t.Context(), owner.Runtime(), sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
	model := tui.New(t.Context(), nil, a, "", func() {}, tui.WithHideSidebar())
	model.Update(messages.ApplySettingsMsg{Preferences: messages.Preferences{InterruptConfirmation: messages.InterruptModeNone}})
	d := tuitest.New(t, model, 100, 30, tuitest.WithTimeout(5*time.Second))
	d.Type("first prompt").Enter()
	<-p.started
	d.WaitFor(tuitest.Contains("first prompt"))
	d.Press(27)
	d.Type("second prompt").Enter()
	d.WaitFor(tuitest.Contains("second prompt"))
	d.WaitFor(tuitest.Contains("second answer"))
	d.WaitFor(tuitest.Contains("Type your message"))
	require.Eventually(t, func() bool {
		st, e := a.SessionHandle().Status(t.Context())
		return e == nil && st.State == runtime.SessionStateSettled
	}, 5*time.Second, time.Millisecond)
}
