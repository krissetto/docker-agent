package messages

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestReplyHandoffRuntimeIntegrationSharesPaintHitsAndStreamingOpacity(t *testing.T) {
	scheduler := &replyHandoffScheduler{now: testHandoffTime()}
	runtime := animation.NewRuntimeWithScheduler(scheduler)
	m := NewScrollableView(runtime, 84, 50, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); runtime.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(messages.VisibleRepliesThree)

	user := types.User("request")
	retiring := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("retiring answer line\n", 17))
	retained := types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("retained prose moves smoothly\n", 6))
	other := types.Agent(types.MessageTypeAssistant, "root", "other retained answer")
	for _, msg := range []*types.Message{user, retiring, retained, other} {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, m.createMessageView(msg))
	}
	m.responseGroups[retiring], m.responseGroups[retained], m.responseGroups[other] = 1, 2, 3
	m.nextResponseGroup = 3
	m.workProjectionDirty = true
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	require.Empty(t, m.workHeaderBySource, "the initial three logical answers are retained outside Work")
	m.userHasScrolled = false
	m.scrollOffset = max(0, m.totalHeight-m.height)
	m.scrollview.SetScrollOffset(m.scrollOffset)
	m.View()

	incomingText := "newly streaming prose"
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", incomingText))
	incoming := m.messages[len(m.messages)-1]
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase())
	require.NotContains(t, ansi.Strip(m.View()), incomingText, "one-shot target content is concealed before its fade")
	incomingID := m.replyHandoff.batch.incoming

	phaseDeadline := runtime.Now() + m.replyHandoff.transition.Duration() - m.replyHandoff.transition.Elapsed()
	for range 3 {
		m.AppendToLastMessage("root", " streamed-chunk")
	}
	require.Contains(t, incoming.Content, "streamed-chunk", "source ingestion is immediate")
	require.Contains(t, strings.Join(layoutTextForComponent(m.replyHandoff.targetLayout, incomingID), "\n"), "streamed-chunk", "the concealed target tracks every accepted chunk")
	require.LessOrEqual(t, runtime.Now()+m.replyHandoff.transition.Duration()-m.replyHandoff.transition.Elapsed(), phaseDeadline+animation.TickRate, "chunks never extend the fixed deadline")

	retiringID := responseHandoffComponent(m.replyHandoff.batch.incoming.Exchange, 1, retiring)
	fadeRemaining := m.replyHandoff.transition.Duration() - m.replyHandoff.transition.Elapsed()
	m.SetSize(m.width-6, m.height)
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "resize preserves the active phase")
	require.Equal(t, fadeRemaining, m.replyHandoff.transition.Duration()-m.replyHandoff.transition.Elapsed(), "resize does not restart or advance the deadline")
	require.Contains(t, sourceMessages(m.replyHandoff.Rows()), retiring, "the retiring source remains mapped during a fade-out resize")
	for _, row := range m.replyHandoff.Rows() {
		if row.Component == retiringID {
			require.InDelta(t, 1.0, row.Opacity, 1e-9, "resize does not flash or discard the retiree before the next accepted tick")
			break
		}
	}

	advanceModelHandoffPhase(t, runtime, m, replyHandoffFadeOut)
	require.Equal(t, replyHandoffReflow, m.replyHandoff.Phase())
	var survivorY []int
	for i := 0; m.replyHandoff.Phase() == replyHandoffReflow && i < 100; i++ {
		acceptAndUpdateHandoffTick(t, runtime, m)
		m.applyReplyHandoffPaint()
		for _, row := range m.replyHandoffFrameRows {
			if row.Source == retained && row.Interactive {
				first, _ := replyHandoffTerminalSpan(row.Y, row.Height)
				survivorY = append(survivorY, first)
				break
			}
			if row.Composite != nil && row.CompositeLine == 0 {
				require.Contains(t, ansi.Strip(row.Text), "Work", "the new composite header stays source-mapped during reflow")
			}
		}
	}
	positions := slices.Compact(slices.Clone(survivorY))
	require.GreaterOrEqual(t, len(positions), 3, "surviving prose must pass through multiple terminal rows: %v", survivorY)

	for _, row := range m.replyHandoffFrameRows {
		first, last := replyHandoffTerminalSpan(row.Y, row.Height)
		if row.Spacer {
			require.False(t, row.Interactive)
			for line := first; line < last; line++ {
				got, local := m.globalLineToMessageLineCached(line)
				require.Equal(t, -1, got, "spacer rows cannot fall through to canonical transcript hits")
				require.Equal(t, -1, local)
			}
			continue
		}
		if row.Source == nil || !row.Interactive || row.Height <= 0 {
			continue
		}
		for line := first; line < last; line++ {
			winner, ok := m.replyHandoffRowAt(line)
			require.True(t, ok)
			got, local := m.globalLineToMessageLineCached(line)
			require.Equal(t, m.messagePointerIndex(winner.Source), got, "hit mapping follows the exact row painted last")
			require.Equal(t, winner.LocalRow, local)
			require.Equal(t, ansi.Strip(winner.Text), ansi.Strip(m.renderedLine(line)), "painted text and source map agree")
		}
	}
	require.Equal(t, replyHandoffFadeIn, m.replyHandoff.Phase())
	previousOpacity := opacityForSource(m.replyHandoff.Rows(), incoming)
	deadline := runtime.Now() + m.replyHandoff.transition.Duration() - m.replyHandoff.transition.Elapsed()
	for i := 0; i < 8 && m.replyHandoff.Active(); i++ {
		m.AppendToLastMessage("root", " live-fade-chunk")
		acceptAndUpdateHandoffTick(t, runtime, m)
		opacity := opacityForSource(m.replyHandoff.Rows(), incoming)
		require.GreaterOrEqual(t, opacity, previousOpacity, "new chunks inherit the current shared component alpha")
		previousOpacity = opacity
	}
	require.LessOrEqual(t, runtime.Now()+m.replyHandoff.transition.Duration()-m.replyHandoff.transition.Elapsed(), deadline+animation.TickRate, "chunk ingestion never moves the finite deadline")
	for range 100 {
		m.AppendToLastMessage("root", " x")
	}
	require.Equal(t, 100, strings.Count(incoming.Content, " x"))
	require.Positive(t, strings.Count(incoming.Content, " live-fade-chunk"), "streaming continued during the one component fade")
	advanceModelHandoffPhase(t, runtime, m, replyHandoffFadeIn)
	require.False(t, m.replyHandoff.Active(), "the fixed fade settles independently of continued stream chunks")
	require.Zero(t, runtime.ActiveCount(), "terminal handoff releases its only lease")
	require.Contains(t, ansi.Strip(m.View()), "streamed-chunk")
}

func TestReplyHandoffTerminalSpanQuantizesFractionalPaintAndHitsIdentically(t *testing.T) {
	first, last := replyHandoffTerminalSpan(1.8, 0.4)
	require.Equal(t, 2, first)
	require.Equal(t, 3, last)
	first, last = replyHandoffTerminalSpan(1.2, 0.3)
	require.Equal(t, 1, first)
	require.Equal(t, 2, last, "sub-cell rows retain one shared terminal hit cell")
}

func testHandoffTime() time.Time { return time.Unix(10, 0) }

func acceptAndUpdateHandoffTick(t *testing.T, runtime *animation.Runtime, m *model) {
	t.Helper()
	cmd := runtime.Continue()
	require.NotNil(t, cmd)
	raw, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	accepted, ok := runtime.Accept(raw)
	require.True(t, ok)
	_, updateCmd := m.Update(accepted)
	if updateCmd != nil {
		_ = updateCmd
	}
}

func advanceModelHandoffPhase(t *testing.T, runtime *animation.Runtime, m *model, phase replyHandoffPhase) {
	t.Helper()
	for i := 0; m.replyHandoff.Phase() == phase && i < 100; i++ {
		acceptAndUpdateHandoffTick(t, runtime, m)
	}
	require.NotEqual(t, phase, m.replyHandoff.Phase(), "finite phase settles within a bounded number of fake-clock ticks")
}

func layoutTextForComponent(layout []replyHandoffLayoutRow, component replyHandoffComponentID) []string {
	var text []string
	for _, row := range layout {
		if row.Component == component {
			text = append(text, row.Text)
		}
	}
	return text
}

var _ tea.Msg = animation.TickMsg{}

func TestReplyHandoffResumeKeepsComponentOpacityAndDeadline(t *testing.T) {
	scheduler := &replyHandoffScheduler{now: testHandoffTime()}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewScrollableView(ar, 80, 20, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.AppendToLastMessage("root", "streamed reply")
	incoming := m.messages[len(m.messages)-1]
	acceptAndUpdateHandoffTick(t, ar, m)
	before := opacityForSource(m.replyHandoff.Rows(), incoming)
	require.Greater(t, before, 0.0)
	require.Less(t, before, 1.0)
	deadline := ar.Now() + m.replyHandoff.transition.Duration() - m.replyHandoff.transition.Elapsed()
	m.StopAnimations()
	require.Zero(t, ar.ActiveCount(), "hidden pages own no animation leases")
	m.ResumeAnimations()
	require.InDelta(t, before, opacityForSource(m.replyHandoff.Rows(), incoming), 1e-9, "resume cannot restart the component fade")
	require.Equal(t, deadline, ar.Now()+m.replyHandoff.transition.Duration()-m.replyHandoff.transition.Elapsed())
	advanceModelHandoffPhase(t, ar, m, replyHandoffFadeIn)
	require.Zero(t, ar.ActiveCount())
}

func TestReplyHandoffKeepsScrolledReaderOnRetainedSource(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 80, 8, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	for _, content := range []string{
		strings.Repeat("retiring paragraph\n\n", 20),
		strings.Repeat("reading retained paragraph\n\n", 12),
		"last reply",
	} {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", content))
	}
	m.ensureAllItemsRendered()
	m.suppressReplyHandoff = false
	m.userHasScrolled = true
	m.scrollOffset = m.lineOffsets[2] + 2
	require.LessOrEqual(t, m.scrollOffset, m.totalScrollableHeight()-m.height, "fixture anchor must not be clamped before the handoff")
	m.scrollview.SetScrollOffset(m.scrollOffset)
	m.View()
	anchorSource, anchorLocal := m.globalLineToMessageLineCached(m.scrollOffset)
	require.Equal(t, 2, anchorSource)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "incoming"))
	for range 100 {
		m.View()
		gotSource, gotLocal := m.globalLineToMessageLineCached(m.scrollOffset)
		require.Equal(t, anchorSource, gotSource, "a scrolled reader remains on the same retained message")
		require.Equal(t, anchorLocal, gotLocal, "a scrolled reader's local line stays anchored")
		if !m.replyHandoff.Active() {
			break
		}
		acceptAndUpdateHandoffTick(t, ar, m)
	}
	require.False(t, m.replyHandoff.Active())
}
