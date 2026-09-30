package chat

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
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
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type lockedBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	sidebar string
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type pendingQueueProbe struct{ reply chan int }

func (b *lockedBuffer) SidebarString() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sidebar
}

func standaloneQueueRows(view, text string) int {
	count := 0
	for row := range strings.SplitSeq(view, "\n") {
		if strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(row), "✎ ×")) == "- "+text {
			count++
		}
	}
	return count
}

func programPendingQueueLength(t *testing.T, program *tea.Program) int {
	t.Helper()
	reply := make(chan int, 1)
	program.Send(pendingQueueProbe{reply: reply})
	select {
	case n := <-reply:
		return n
	case <-time.After(time.Second):
		t.Fatal("pending queue probe timed out")
		return -1
	}
}

type teaPageModel struct {
	page  *chatPage
	frame *lockedBuffer
}

func (m teaPageModel) Init() tea.Cmd { return tea.Batch(m.page.Init(), m.page.ar.Continue()) }
func (m teaPageModel) Update(msg tea.Msg) (nextModel tea.Model, nextCmd tea.Cmd) {
	defer func() { nextCmd = tea.Batch(nextCmd, m.page.ar.Continue()) }()
	if tick, ok := msg.(animation.TickMsg); ok {
		accepted, current := m.page.ar.Accept(tick)
		if !current {
			return m, nil
		}
		updated, cmd := m.page.Update(accepted)
		m.page = updated.(*chatPage)
		return m, cmd
	}

	if probe, ok := msg.(pendingQueueProbe); ok {
		probe.reply <- m.page.QueueLength()
		return m, nil
	}
	var next layout.Model
	var cmd tea.Cmd
	next, cmd = m.page.Update(msg)
	m.page = next.(*chatPage)
	return m, cmd
}

func (m teaPageModel) View() tea.View {
	view := m.page.View()
	m.frame.mu.Lock()
	m.frame.b.Reset()
	_, _ = m.frame.b.WriteString(view)
	m.frame.sidebar = m.page.sidebar.View()
	m.frame.mu.Unlock()
	return tea.NewView(view)
}

type programDispatchProvider struct {
	mu      sync.Mutex
	streams []chat.MessageStream
	calls   int
}

func (*programDispatchProvider) ID() modelsdev.ID {
	return modelsdev.ParseIDOrZero("test/program-dispatch")
}
func (*programDispatchProvider) BaseConfig() base.Config { return base.Config{} }
func (*programDispatchProvider) MaxTokens() int          { return 0 }
func (p *programDispatchProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

func (p *programDispatchProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type programDispatchStream struct {
	started  chan struct{}
	release  chan struct{}
	content  string
	finished chan struct{}
	once     sync.Once
	endOnce  sync.Once
	index    int
}

func (s *programDispatchStream) Recv() (chat.MessageStreamResponse, error) {
	switch s.index {
	case 0:
		s.index++
		s.once.Do(func() { close(s.started) })
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{Content: s.content}}}}, nil
	case 1:
		s.index++
		<-s.release
		s.endOnce.Do(func() {
			if s.finished != nil {
				close(s.finished)
			}
		})
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}
func (*programDispatchStream) Close() {}

func TestActualProgramSettlingRunAutomaticallyDispatchesQueuedFIFO(t *testing.T) {
	aStarted := make(chan struct{})
	aRelease := make(chan struct{})
	bStarted := make(chan struct{})
	bFinished := make(chan struct{})
	cStarted := make(chan struct{})
	cFinished := make(chan struct{})
	cRelease := make(chan struct{})
	bRelease := make(chan struct{})
	bStream := &programDispatchStream{started: bStarted, release: bRelease, finished: bFinished, content: "B streamed"}
	cStream := &programDispatchStream{started: cStarted, release: cRelease, finished: cFinished, content: "C streamed"}
	provider := &programDispatchProvider{streams: []chat.MessageStream{
		&programDispatchStream{started: aStarted, release: aRelease, content: "A streamed"},
		bStream,
		cStream,
	}}
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(provider)),
	)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sess := session.New(session.WithID("program-dispatch"), session.WithAgentName("root"), session.WithTitle("dispatch"), session.WithNonInteractive(true))
	a := app.New(t.Context(), rt, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
	page := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	frame := &lockedBuffer{}
	output := &lockedBuffer{}
	program := tea.NewProgram(teaPageModel{page: page, frame: frame}, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(output), tea.WithWindowSize(140, 40))
	done := make(chan error, 1)
	go func() { _, runErr := program.Run(); done <- runErr }()
	t.Cleanup(func() {
		program.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("program did not stop")
		}
	})
	go a.Subscribe(t.Context(), func(msg any) { program.Send(msg) }, app.SubscribeOptions{})
	a.Start(t.Context())
	require.Eventually(t, func() bool { return frame.String() != "" }, 3*time.Second, time.Millisecond)

	// Submit through the real App/session path while the actual Program renders
	// only canonical runtime events. Once B is accepted, releasing provider A
	// is the sole cause of every subsequent transition.
	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	a.Run(runCtx, cancelRun, "A", nil)
	select {
	case <-aStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("A provider invocation did not start")
	}
	_, err = a.FollowUpMessage(t.Context(), "B queued", nil)
	require.NoError(t, err)
	_, err = a.FollowUpMessage(t.Context(), "C queued", nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, statusErr := a.SessionHandle().Status(t.Context())
		return statusErr == nil && status.Pending == 2 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "B queued") == 1 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "C queued") == 1 && provider.count() == 1
	}, 3*time.Second, time.Millisecond, "queued inputs must render before A settles")

	close(aRelease) // no Program.Send, key, command injection, or external wake after this point
	select {
	case <-bStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("A settlement did not automatically invoke B")
	}
	require.Equal(t, 2, provider.count(), "B starts solely from A settlement")
	require.Eventually(t, func() bool {
		view := ansi.Strip(frame.String())
		status, statusErr := a.SessionHandle().Status(t.Context())
		return statusErr == nil && status.Pending == 1 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "C queued") == 1 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "B queued") == 0 && strings.Contains(view, "B streamed")
	}, 3*time.Second, time.Millisecond)
	close(bRelease) // B settlement is the sole trigger for C
	select {
	case <-cStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("B settlement did not automatically invoke C")
	}
	require.Eventually(t, func() bool {
		return strings.Contains(frame.String(), "C streamed")
	}, 3*time.Second, time.Millisecond)
	close(cRelease)
	select {
	case <-cFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("C stream did not reach EOF")
	}
	require.Eventually(t, func() bool {
		status, statusErr := a.SessionHandle().Status(t.Context())
		// Backend settlement precedes the sidebar's finite queue-exit fade.
		// Wait for that presentation boundary too before counting the whole
		// page: a fading queue copy is not a duplicate transcript admission.
		sidebar := ansi.Strip(frame.SidebarString())
		return statusErr == nil && status.State == runtime.SessionStateSettled && status.Pending == 0 &&
			!strings.Contains(sidebar, "B queued") && !strings.Contains(sidebar, "C queued")
	}, 5*time.Second, time.Millisecond)
	assert.Equal(t, 3, provider.count(), "A, B, and C each invoke the provider exactly once")
	view := ansi.Strip(frame.String())
	assert.Equal(t, 1, strings.Count(view, "B queued"), "B appears in chat exactly once")
	assert.Equal(t, 1, strings.Count(view, "C queued"), "C appears in chat exactly once")
	assert.NotEmpty(t, output.String(), "actual tea.Program rendered output")
}

func TestProgramRendersCanonicalPendingPromotion(t *testing.T) {
	sess := session.New(session.WithID("render-pending"), session.WithAgentName("root"))
	a, _ := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning})
	page := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	page.working = true
	out := &lockedBuffer{}
	frame := &lockedBuffer{}
	program := tea.NewProgram(teaPageModel{page: page, frame: frame}, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(out), tea.WithWindowSize(140, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	t.Cleanup(func() {
		program.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("program did not stop")
		}
	})

	program.Send(runtime.PendingUserMessageAccepted(sess.ID, "queued-turn", "queued visible", nil, 1))
	require.Eventually(t, func() bool {
		return programPendingQueueLength(t, program) == 1 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "queued visible") == 1
	}, 3*time.Second, 10*time.Millisecond)

	program.Send(runtime.PendingUserMessagePromoted(sess.ID, "queued-turn", "queued visible", nil, 1))
	require.Eventually(t, func() bool {
		view := ansi.Strip(frame.String())
		return programPendingQueueLength(t, program) == 0 && standaloneQueueRows(ansi.Strip(frame.SidebarString()), "queued visible") == 0 && strings.Count(view, "queued visible") == 1
	}, 3*time.Second, 10*time.Millisecond)
	assert.NotEmpty(t, out.String(), "actual tea.Program rendered frames")

	program.Send(runtime.StreamStarted(sess.ID, "root"))
	program.Send(runtime.StreamStopped(sess.ID, "root", "normal"))
}
