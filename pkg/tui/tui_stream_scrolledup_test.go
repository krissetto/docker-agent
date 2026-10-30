package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func rootFrameWidths(frame string) []int {
	lines := strings.Split(frame, "\n")
	widths := make([]int, len(lines))
	for i, line := range lines {
		widths[i] = ansi.StringWidth(line)
	}
	return widths
}

func TestActualProgramScrolledUpStreamDefersOffscreenTail(t *testing.T) {
	// The fixture's first start message begins a reply handoff whose fade is
	// driven by animation ticks. The fixture discards command results, so the
	// wall-clock tick scheduled by that first Continue is never delivered and a
	// wall-clock runtime would stay at t=0 forever. A test-owned scheduler keeps
	// the lease and lets the test deliver it.
	sched := &manualTickScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(t, 120, 40, animation.NewRuntimeWithScheduler(sched))
	sess, _, _ := mixedHistorySession(1000)
	root.application.Session().Messages = sess.Messages
	_ = root.chatPage.Init()
	root.handleWindowResize(120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", "start\n\n")})
	_ = root.View()
	root.chatPage.ScrollToBottom()
	_, _ = root.Update(messages.WheelCoalescedMsg{Delta: -3, X: 30, Y: 15})
	stable := root.View().Content

	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	wrapped := &scrolledUpStreamModel{streamingMotionModel: model}
	program := startTestProgram(t, root, wrapped, tea.WithOutput(writer))
	<-model.ready
	driveReplyHandoffToIdle(t, program, sched)
	waitForProgramQuiescence(t, model, writer)
	baselineWrites := writer.writes.Load()
	baselineCompositions := model.compositions.Load()

	chunk := "offscreen **markdown** [link](https://example.com)\n\n"
	for i := range 200 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
		program.Send(tea.MouseMotionMsg{X: 30 + i%2, Y: 10})
	}
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, uint64(200), model.chunks.Load())
	require.Equal(t, uint64(200), model.motions.Load())
	content := make(chan string)
	program.Send(streamingMotionRead{content: content})
	visible := <-content
	require.Equal(t, strings.Count(stable, "\n"), strings.Count(visible, "\n"), "offscreen stream and hover must not change viewport line count")
	require.Equal(t, rootFrameWidths(stable), rootFrameWidths(visible), "offscreen stream and hover must not change viewport widths/wrapping")
	require.LessOrEqual(t, model.compositions.Load()-baselineCompositions, uint64(2))
	require.LessOrEqual(t, writer.writes.Load()-baselineWrites, uint64(2))

	// Stream stop is an exact-content boundary even while the viewport remains
	// scrolled up. The stop's ScrollToBottom command is intentionally ignored
	// by the component in this state, so this proves finalization itself
	// reconciles the buffered tail. A later End only exposes already-finalized
	// content; it must not be the operation that makes it exact.
	program.Send(agentruntime.StreamStopped("profile", "root", "normal"))
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	content = make(chan string)
	program.Send(streamingMotionRead{content: content})
	require.NotContains(t, <-content, "offscreen", "stream stop must not jump the scrolled-up viewport to the tail")

	program.Send(messages.WheelCoalescedMsg{Delta: 1_000_000, X: 30, Y: 15})
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	content = make(chan string)
	program.Send(streamingMotionRead{content: content})
	require.Contains(t, <-content, "offscreen", "End must reveal exact content finalized at stream stop")
}

// manualTickScheduler delivers animation ticks only when the test asks, one
// frame at a time, so the reply handoff advances deterministically.
type manualTickScheduler struct {
	mu      sync.Mutex
	now     time.Time
	pending func(time.Time) tea.Msg
}

func (s *manualTickScheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *manualTickScheduler) Tick(_ time.Duration, createMsg func(time.Time) tea.Msg) tea.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = createMsg
	return nil
}

// deliverNext returns the outstanding tick lease as a message, or nil when the
// runtime holds no lease.
func (s *manualTickScheduler) deliverNext() tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil
	}
	s.now = s.now.Add(animation.TickRate)
	create := s.pending
	s.pending = nil
	return create(s.now)
}

// handoffIdleRead asks the program goroutine whether the reply handoff is idle.
type handoffIdleRead struct{ idle chan bool }

type scrolledUpStreamModel struct{ *streamingMotionModel }

func (d *scrolledUpStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if read, ok := msg.(handoffIdleRead); ok {
		d.mu.Lock()
		defer d.mu.Unlock()
		idle := false
		if owner, ok := d.root.chatPage.(interface{ ReplyHandoffActive() bool }); ok {
			idle = !owner.ReplyHandoffActive()
		}
		read.idle <- idle
		return d, nil
	}
	_, cmd := d.streamingMotionModel.Update(msg)
	return d, cmd
}

// driveReplyHandoffToIdle delivers one tick at a time and waits for the program
// to process each one before checking the handoff. Each delivery is followed by
// a request/reply on the program goroutine, so the check is ordered after the
// tick's Update.
func driveReplyHandoffToIdle(t *testing.T, program *tea.Program, sched *manualTickScheduler) {
	t.Helper()
	const maxFrames = 1000
	for frame := 0; frame <= maxFrames; frame++ {
		idle := make(chan bool, 1)
		program.Send(handoffIdleRead{idle: idle})
		if <-idle {
			return
		}
		tick := sched.deliverNext()
		require.NotNil(t, tick, "reply handoff is active but the animation runtime holds no tick lease")
		program.Send(tick)
	}
	t.Fatalf("reply handoff did not reach idle within %d frames", maxFrames)
}
