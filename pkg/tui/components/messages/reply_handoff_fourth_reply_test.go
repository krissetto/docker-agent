package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// newFourthReplyFixture: user request, three committed prose replies, compact
// Work on, Last3 policy, rendered so renderDirty is false. The fourth reply is
// delivered by each test.
func newFourthReplyFixture(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 84, 50, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesThree)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	for i := range 3 {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("seed reply %d prose", i)))
		m.committedMessageCount = len(m.messages)
	}
	m.suppressReplyHandoff = false
	m.View()
	m.syncWorkProjection()
	require.Len(t, m.workProjection.exchanges, 1)
	require.Len(t, m.workProjection.exchanges[0].response, 3, "precondition: three Last3 prose replies visible")
	require.Empty(t, m.workProjection.exchanges[0].work)
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	return m
}

func fourthReplyOldestGroup(m *model) uint64 { return m.responseGroups[m.messages[1]] }

func fourthReplyRetiringGroups(m *model) []uint64 {
	var groups []uint64
	for _, id := range m.replyHandoff.batch.retiring {
		groups = append(groups, id.Group)
	}
	return groups
}

// A spinner tail, then a media-only assistant turn, then the first prose chunk:
// the removed spinner dirties rendering before the pending snapshot, so the
// oldest reply must still be captured and retired.
func TestReplyHandoffSpinnerMediaFirstProseRetiresOldest(t *testing.T) {
	m := newFourthReplyFixture(t)
	oldest := fourthReplyOldestGroup(m)
	call := tools.ToolCall{ID: "fourth-reply-media-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	m.AddAssistantMessage("root", "")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{Fallback: "fourth reply media"}})
	m.AddAssistantMessage("root", "")
	m.View()
	require.False(t, m.renderDirty, "precondition: rendered before the first prose chunk")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	m.AppendToLastMessage("root", "fourth reply prose")
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "a dropped pending capture must not drop the retiree")
	require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
	require.Equal(t, animation.ShortDuration/2, m.replyHandoff.transition.Duration(), "fade-out runs 175ms")
}

// A whitespace-only first chunk creates a prose-less assistant; the first prose
// chunk must count as incoming and retire the oldest reply.
func TestReplyHandoffWhitespaceFirstProseRetiresOldest(t *testing.T) {
	m := newFourthReplyFixture(t)
	oldest := fourthReplyOldestGroup(m)
	call := tools.ToolCall{ID: "fourth-reply-ws-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	m.AddAssistantMessage("root", "")
	m.AppendToLastMessage("root", "\n\n")
	m.AddAssistantMessage("root", "")
	m.View()
	require.False(t, m.renderDirty, "precondition: rendered before the first prose chunk")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	m.AppendToLastMessage("root", "fourth reply prose")
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase())
	require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
}

// Deferred chunks (scrolled away) take the materialize path; the pre-mutation
// snapshot must still retire the oldest reply.
func TestReplyHandoffDeferredFirstProseRetiresOldest(t *testing.T) {
	m := newFourthReplyFixture(t)
	oldest := fourthReplyOldestGroup(m)
	call := tools.ToolCall{ID: "fourth-reply-deferred-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	// A real prose-less assistant placeholder; AddAssistantMessage would add a spinner.
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	m.userHasScrolled = true
	m.AppendToLastMessage("root", "fourth reply prose")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase(), "deferred chunks start no handoff")
	m.userHasScrolled = false
	m.scrollToBottom()
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase())
	require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
}

// Keyboard message selection keeps pinned replies until it ends; a clear of the
// mouse selection alone must not release them.
func TestReplyHandoffKeyboardSelectionHoldsRetainedReplies(t *testing.T) {
	m := newFourthReplyFixture(t)
	m.suppressReplyHandoff = true
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "fourth reply prose"))
	m.suppressReplyHandoff = false
	m.View()
	m.selectedMessageIndex = 1
	m.workRetained[m.messages[1]] = struct{}{}
	m.workProjectionDirty = true
	m.renderDirty = true
	m.View()
	m.clearSelection()
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase(), "keyboard selection still holds the pin")
	require.Contains(t, fourthReplyResponseSources(m), 1)
	m.Blur()
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "blur ends the selection and releases the pin")
	require.Empty(t, m.workRetained)
}

func fourthReplyResponseSources(m *model) []int {
	var sources []int
	for _, exchange := range m.workProjection.exchanges {
		sources = append(sources, exchange.response...)
	}
	return sources
}

// Replies pinned by selection stay visible; every selection-clear path releases
// them through the normal phased handoff.
func TestReplyHandoffSelectionClearReleasesRetainedReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*model)
	}{
		{"clearSelection", func(m *model) { m.clearSelection() }},
		{"ClearPresentationSelection", func(m *model) { m.ClearPresentationSelection() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFourthReplyFixture(t)
			oldest := fourthReplyOldestGroup(m)
			m.suppressReplyHandoff = true
			m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "fourth reply prose"))
			m.suppressReplyHandoff = false
			m.View()
			// Selection changes dirty rendering; mirror that before the pin takes effect.
			m.workRetained[m.messages[1]] = struct{}{}
			m.workProjectionDirty = true
			m.renderDirty = true
			m.View()
			require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
			require.Contains(t, fourthReplyResponseSources(m), 1, "a pinned reply stays visible while selection retains it")
			tc.run(m)
			require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "release starts the normal fade-out")
			require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
			require.Empty(t, m.workRetained)
		})
	}
}

// Fixed phase clocks: fade-out 175ms, reflow 250ms, fade-in 175ms.
func TestReplyHandoffPhaseDurationsAreExact(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	h := newReplyHandoff(ar)
	t.Cleanup(func() { h.Stop(); ar.Stop() })
	for _, tc := range []struct {
		phase replyHandoffPhase
		want  time.Duration
	}{
		{replyHandoffFadeOut, 175 * time.Millisecond},
		{replyHandoffReflow, 250 * time.Millisecond},
		{replyHandoffFadeIn, 175 * time.Millisecond},
	} {
		h.startPhase(tc.phase)
		require.Equal(t, tc.want, h.transition.Duration(), "phase %v", tc.phase)
	}
}

// Promoted from the fourth-reply probe: a streamed bubble followed by reasoning
// and final content is one reply. Live must match the replayed record: one reply
// group, and one prose bubble carrying the final text.
func TestFinalizeStreamedTailReusesStreamedBubbleBeforeReasoning(t *testing.T) {
	ar := animation.NewRuntime()
	live := NewScrollableView(ar, 80, 40, &service.SessionState{}).(*model)
	t.Cleanup(func() { live.StopAnimations(); ar.Stop() })
	live.SetCompactWorkLog(true)
	live.SetVisibleReplies(msgtypes.VisibleRepliesThree)
	live.suppressReplyHandoff = true
	live.AddUserMessage("request")
	live.AppendToLastMessage("root", "alpha prose")
	live.AppendReasoning("root", "thinking")
	live.FinalizeStreamedAssistant("root", "alpha prose and final", "thinking", false)
	liveGroups, liveTexts := fourthReplyProseStats(live)

	sess := session.New()
	sess.AddMessage(session.UserMessage("request"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		Content:          "alpha prose and final",
		ReasoningContent: "thinking",
	}))
	ar2 := animation.NewRuntime()
	replay := NewScrollableView(ar2, 80, 40, &service.SessionState{}).(*model)
	t.Cleanup(func() { replay.StopAnimations(); ar2.Stop() })
	replay.SetCompactWorkLog(true)
	replay.SetVisibleReplies(msgtypes.VisibleRepliesThree)
	replay.LoadFromSession(sess, nil)
	replayGroups, replayTexts := fourthReplyProseStats(replay)

	require.Len(t, liveGroups, len(replayGroups), "one stored reply record maps to one live reply group")
	require.Equal(t, replayTexts, liveTexts)
	require.Equal(t, []string{"alpha prose and final"}, liveTexts)
}

// fourthReplyProseStats returns the distinct reply groups and prose texts of the
// visible prose responses.
func fourthReplyProseStats(m *model) (map[uint64]struct{}, []string) {
	m.syncWorkProjection()
	groups := make(map[uint64]struct{})
	var texts []string
	for _, exchange := range m.workProjection.exchanges {
		for _, source := range exchange.response {
			msg := m.messages[source]
			if msg.Type != types.MessageTypeAssistant || strings.TrimSpace(msg.Content) == "" {
				continue
			}
			groups[m.responseGroups[msg]] = struct{}{}
			texts = append(texts, msg.Content)
		}
	}
	return groups, texts
}

// fourthReplyTextCoord returns the screen cell of needle in the current frame and
// the global line it sits on.
func fourthReplyTextCoord(t *testing.T, m *model, needle string) (x, y, line int) {
	t.Helper()
	for row, plain := range strings.Split(ansi.Strip(m.View()), "\n") {
		if before, _, ok := strings.Cut(plain, needle); ok {
			return m.xPos + ansi.StringWidth(before) + 1, m.yPos + row, m.scrollOffset + row
		}
	}
	require.FailNow(t, "text not visible in frame", needle)
	return 0, 0, 0
}

// A plain click ends the held selection: the pinned oldest reply is released
// through the normal fade, and the scrolled reader stays on its retained source.
func TestReplyHandoffPlainClickReleasesPinAndKeepsScrolledReader(t *testing.T) {
	m := newFourthReplyFixture(t)
	oldest := fourthReplyOldestGroup(m)
	call := tools.ToolCall{ID: "fourth-reply-click-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	// A real prose-less assistant placeholder; AddAssistantMessage would add a spinner.
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	x, y, anchorLine := fourthReplyTextCoord(t, m, "seed reply 1 prose")
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, m.selection.active, "the press starts a held selection")
	m.AppendToLastMessage("root", "fourth reply prose")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase(), "a held selection suppresses the handoff")
	require.Contains(t, fourthReplyResponseSources(m), 1, "the pinned oldest reply stays visible")

	// Shrink only after the press so the anchored reply is still on screen at press
	// time. Height 3 keeps the reader above the bottom clamp after the retiree
	// collapses; at the bottom the shrinking document moves the reader by design.
	m.SetSize(84, 3)
	m.userHasScrolled = true
	m.setScrollOffset(anchorLine)
	m.View()
	require.Positive(t, m.totalScrollableHeight(), "fixture must scroll")
	require.Equal(t, anchorLine, m.scrollOffset, "fixture must scroll so the anchored reply is the top row")
	anchorSource, anchorLocal := m.globalLineToMessageLineCached(m.scrollOffset)

	m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: m.yPos + anchorLine - m.scrollOffset, Button: tea.MouseLeft})
	require.False(t, m.selection.active, "the plain click ends the selection")
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "the release starts the normal fade-out")
	require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
	require.Empty(t, m.workRetained)

	// The anchored reply is retained, not the retiree, so the reader must stay on it through every tick.
	for range 100 {
		if !m.replyHandoff.Active() {
			break
		}
		m.View()
		gotSource, gotLocal := m.globalLineToMessageLineCached(m.scrollOffset)
		require.Equal(t, anchorSource, gotSource, "a scrolled reader remains on the same retained reply")
		require.Equal(t, anchorLocal, gotLocal, "a scrolled reader's local line stays anchored")
		acceptAndUpdateHandoffTick(t, m.ar, m)
	}
	require.False(t, m.replyHandoff.Active())
	m.View()
	finalSource, finalLocal := m.globalLineToMessageLineCached(m.scrollOffset)
	require.Equal(t, anchorSource, finalSource, "handoff completion keeps the reader on the retained reply")
	require.Equal(t, anchorLocal, finalLocal, "handoff completion keeps the local line anchored")
	maxOffset := max(0, m.totalScrollableHeight()-m.height)
	require.LessOrEqual(t, m.scrollOffset, maxOffset, "completion stays within the legal scroll range")
	require.GreaterOrEqual(t, m.scrollOffset, 0)
	require.NotContains(t, fourthReplyResponseSources(m), 1, "the released reply leaves the response set")
}

// Scroll-to-bottom must not end a held selection, but it drains a pin whose
// selection was cleared without a release hook.
func TestReplyHandoffScrollToBottomKeepsHeldPinUntilSelectionEnds(t *testing.T) {
	m := newFourthReplyFixture(t)
	oldest := fourthReplyOldestGroup(m)
	call := tools.ToolCall{ID: "fourth-reply-bottom-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	// A real prose-less assistant placeholder; AddAssistantMessage would add a spinner.
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	x, y, _ := fourthReplyTextCoord(t, m, "seed reply 1 prose")
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.AppendToLastMessage("root", "fourth reply prose")
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())

	m.scrollToBottom()
	m.View()
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase(), "scroll-to-bottom does not end a held selection")
	require.Contains(t, fourthReplyResponseSources(m), 1)
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset, "the reader lands on the bottom")

	m.selection.clear()
	m.scrollToBottom()
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase(), "scroll-to-bottom drains a pin whose selection ended")
	require.Equal(t, []uint64{oldest}, fourthReplyRetiringGroups(m))
	require.Empty(t, m.workRetained)

	advanceModelHandoffPhase(t, m.ar, m, replyHandoffFadeOut)
	advanceModelHandoffPhase(t, m.ar, m, replyHandoffReflow)
	advanceModelHandoffPhase(t, m.ar, m, replyHandoffFadeIn)
	require.Equal(t, replyHandoffIdle, m.replyHandoff.Phase())
	require.NotContains(t, fourthReplyResponseSources(m), 1)
	m.View()
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset, "the reader stays at the bottom after the drain")
}

// fourthReplyVisibleSource reports whether any visible row maps to message index source.
func fourthReplyVisibleSource(m *model, source int) bool {
	for sy := range m.height {
		if src, _ := m.globalLineToMessageLineCached(m.scrollOffset + sy); src == source {
			return true
		}
	}
	return false
}

// Retiring the oldest reply can shrink the document below the reader's offset.
// The clamp is then unavoidable: every frame must stay in the legal scroll range,
// the retained reply must stay in view, and completion lands on the legal bottom.
func TestReplyHandoffPlainClickClampsReaderToLegalBottom(t *testing.T) {
	m := newFourthReplyFixture(t)
	call := tools.ToolCall{ID: "fourth-reply-clamp-call", Function: tools.FunctionCall{Name: "check"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	x, y, anchorLine := fourthReplyTextCoord(t, m, "seed reply 1 prose")
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.AppendToLastMessage("root", "fourth reply prose")
	m.SetSize(84, 6)
	m.userHasScrolled = true
	m.setScrollOffset(anchorLine)
	m.View()
	require.Equal(t, anchorLine, m.scrollOffset, "reader starts on the retained reply")
	require.True(t, fourthReplyVisibleSource(m, 2))

	m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: m.yPos + anchorLine - m.scrollOffset, Button: tea.MouseLeft})
	require.Equal(t, replyHandoffFadeOut, m.replyHandoff.Phase())

	for range 100 {
		if !m.replyHandoff.Active() {
			break
		}
		m.View()
		require.LessOrEqual(t, m.scrollOffset, max(0, m.totalScrollableHeight()-m.height), "every frame stays within the legal scroll range")
		require.True(t, fourthReplyVisibleSource(m, 2), "the retained reply stays in view through the clamp")
		acceptAndUpdateHandoffTick(t, m.ar, m)
	}
	require.False(t, m.replyHandoff.Active())
	m.View()
	bottom := max(0, m.totalScrollableHeight()-m.height)
	require.Equal(t, bottom, m.scrollOffset, "the clamp lands the reader on the legal bottom once the retiree collapses")
	require.True(t, fourthReplyVisibleSource(m, 2))
}
