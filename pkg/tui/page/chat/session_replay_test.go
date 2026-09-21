package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type (
	replayProbe   struct{ reply chan bool }
	replayStart   struct{}
	replayProgram struct {
		page          *chatPage
		gate, started chan struct{}
		gated         bool
		maxUpdate     time.Duration
	}
)

func (m *replayProgram) Init() tea.Cmd { return nil }
func (m *replayProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if q, ok := msg.(replayProbe); ok {
		q.reply <- Loading(m.page)
		return m, nil
	}
	var cmd tea.Cmd
	start := time.Now()
	if _, ok := msg.(replayStart); ok {
		cmd = m.page.Init()
	} else {
		_, cmd = m.page.Update(msg)
	}
	m.maxUpdate = max(m.maxUpdate, time.Since(start))
	if !m.gated && list.ReplayWaiting(m.page.messages) {
		m.gated = true
		close(m.started)
		next := cmd
		gate := m.gate
		cmd = func() tea.Msg { <-gate; return next() }
	}
	return m, core.MapCommand(cmd, nil)
}

func (m *replayProgram) View() tea.View {
	if Loading(m.page) {
		return tea.NewView("Loading...")
	}
	return tea.NewView(m.page.View())
}

func TestReplayProgramOversizedRenderYieldsAndResetMergesEvents(t *testing.T) {
	testOversizedReplay(t, false, false)
}

func TestReplayProgramOversizedReasoningYields(t *testing.T) {
	for _, expanded := range []bool{false, true} {
		t.Run(fmt.Sprintf("expanded=%v", expanded), func(t *testing.T) { testOversizedReplay(t, true, expanded) })
	}
}

func testOversizedReplay(t *testing.T, reasoning, expanded bool) {
	t.Helper()
	sess := session.New(session.WithID("replay"))
	for i := range 1000 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("history %d", i)))
	}
	body := strings.Repeat("# Heading\n\nA **bold** paragraph with `code`.\n\n", 10000)
	message := chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body}
	if reasoning {
		message.Content = ""
		message.ReasoningContent = body
	}
	sess.AddMessage(&session.Message{AgentName: "worker", Message: message})
	a, _ := newSessionTestApp(t, sess, nil, nil)
	state := service.NewSessionState(sess)
	state.SetExpandThinking(expanded)
	p := New(animation.NewRuntime(), t.Context(), a, state).(*chatPage)
	p.SetSize(120, 40)
	EnableAsyncReplay(p)
	m := &replayProgram{page: p, gate: make(chan struct{}), started: make(chan struct{})}
	program := tea.NewProgram(m, tea.WithInput(nil), tea.WithoutRenderer(), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	defer func() { program.Quit(); require.NoError(t, <-done) }()
	program.Send(replayStart{})
	select {
	case <-m.started:
	case <-time.After(3 * time.Second):
		t.Fatal("oversized render not deferred")
	}
	probe := func() bool {
		reply := make(chan bool, 1)
		program.Send(replayProbe{reply: reply})
		select {
		case loading := <-reply:
			return loading
		case <-time.After(200 * time.Millisecond):
			t.Fatal("render blocked UI")
			return false
		}
	}
	require.True(t, probe())
	program.Send(&runtime.UserMessageEvent{Message: "new streamed input", SessionPosition: sess.ItemCount(), TurnID: "new"})
	program.Send(&runtime.UserMessageEvent{Message: "history 0", SessionPosition: 0})
	require.True(t, probe())
	close(m.gate)
	require.Eventually(t, func() bool { return !probe() }, 30*time.Second, 10*time.Millisecond)
	// An authoritative reset takes the same asynchronous path, not ResetFromSession.
	snapshot := sess.Clone()
	snapshot.AddMessage(session.UserMessage("authoritative tail"))
	program.Send(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: snapshot, TranscriptPosition: snapshot.ItemCount()}})
	require.Eventually(t, func() bool { return !probe() }, 30*time.Second, 10*time.Millisecond)
	program.Quit()
	require.NoError(t, <-done)
	done <- nil
	require.Equal(t, 1001, p.messages.MessageTypeCount(types.MessageTypeUser))
	p.messages.SetSize(120, 40)
	require.Contains(t, ansi.Strip(p.messages.View()), "authoritative tail")
	t.Log("maximum owner Update", m.maxUpdate)
	require.Less(t, m.maxUpdate, 200*time.Millisecond)
}
