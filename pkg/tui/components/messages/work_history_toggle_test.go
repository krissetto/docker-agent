package messages

import (
	"fmt"
	"reflect"
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
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func workHistoryLongExchange() *session.Session {
	sess := session.New(session.WithID("work-history-toggle"))
	sess.AddMessage(session.UserMessage("one human request begins this long exchange"))
	for i := range 40 {
		call := tools.ToolCall{ID: fmt.Sprintf("historical-call-%02d", i), Function: tools.FunctionCall{Name: "probe"}}
		result := fmt.Sprintf("PRIVATE_TOOL_RESULT_%02d", i)
		msg := session.NewAgentMessage("worker", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "probe"}},
			Presentation: []chat.AssistantPart{
				{Type: chat.AssistantPartReasoning, Text: fmt.Sprintf("PRIVATE_THOUGHT_%02d", i)},
				{Type: chat.AssistantPartContent, Text: fmt.Sprintf("PRIVATE_INTERMEDIATE_REPLY_%02d", i)},
				{Type: chat.AssistantPartToolCall, ToolCallID: call.ID},
			},
		})
		sess.AddMessage(msg)
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result}))
	}
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
		Role:         chat.MessageRoleAssistant,
		Content:      "FINAL_ANSWER_REMAINS_VISIBLE",
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "FINAL_ANSWER_REMAINS_VISIBLE"}},
	}))
	return sess
}

func replayWorkHistory(t *testing.T, height int) (*model, *session.Session) {
	t.Helper()
	return replayWorkHistorySession(t, height, workHistoryLongExchange())
}

func replayWorkHistorySession(t *testing.T, height int, sess *session.Session) (*model, *session.Session) {
	t.Helper()
	ar := animation.NewRuntime()
	state := &service.SessionState{}
	state.SetExpandThinking(false)
	m := NewScrollableView(ar, 82, height, state).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesLatest)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	return m, sess
}

// workHistoryTallFinal is workHistoryLongExchange with a final answer of twelve
// paragraphs. Its rows alone cover the first frame, so the older 81 items are a
// real prefix to page in. The marker stays on the last paragraph.
func workHistoryTallFinal() *session.Session {
	sess := workHistoryLongExchange()
	paragraphs := make([]string, 0, 12)
	for i := range 11 {
		paragraphs = append(paragraphs, fmt.Sprintf("FINAL_ANSWER_PARAGRAPH_%02d keeps the latest answer several rows tall.", i))
	}
	paragraphs = append(paragraphs, "FINAL_ANSWER_REMAINS_VISIBLE")
	content := strings.Join(paragraphs, "\n\n")
	last := sess.Messages[len(sess.Messages)-1].Message
	last.Message.Content = content
	last.Message.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: content}}
	return sess
}

func TestContinueHistoryKeepsLongHumanExchangeGroupedAndSourceLossless(t *testing.T) {
	m, sess := replayWorkHistorySession(t, 8, workHistoryTallFinal())
	tail := len(m.messages) - 1
	require.Positive(t, tail, "the transcript holds the human request, the grouped tool activity and the final answer")
	require.GreaterOrEqual(t, m.slotBase[tail]+b2i(m.slotSep[tail]), m.height, "the final answer alone covers the first frame")
	require.Equal(t, tail, m.historyStart, "the older transcript items are the prefix paged in by ContinueHistory")
	require.True(t, m.HistoryPending(), "fixture must leave an older prefix to page in")
	initialStart := m.historyStart
	initialWork := len(m.workHeaderBySource)

	originalMessages := append([]*types.Message(nil), m.messages...)
	originalViews := append([]layout.Model(nil), m.views...)
	originalSession := sess.Clone()
	observedWork := initialWork > 0
	for m.HistoryPending() {
		before := m.historyStart
		cmd := ContinueHistory(m)
		if cmd != nil {
			runReplayCommand(m, cmd)
		}
		advanced := before - m.historyStart
		require.Positive(t, advanced, "each ContinueHistory step makes bounded progress")
		require.LessOrEqual(t, advanced, 8, "history yields in bounded source batches")
		m.ensureAllItemsRendered()
		plain := ansi.Strip(strings.Join(m.renderedLines, "\n"))
		if len(m.workHeaderBySource) > 0 {
			observedWork = true
		}
		require.LessOrEqual(t, strings.Count(plain, "Work [+]"), 1, "older source rows never leak into the transcript as peer rows")
		// A collapsed Work block previews at most the three most recent reasoning lines.
		require.LessOrEqual(t, strings.Count(plain, "PRIVATE_THOUGHT_"), 3, "collapsed Work shows only the newest reasoning preview")
		require.NotContains(t, plain, "PRIVATE_INTERMEDIATE_REPLY_")
		require.NotContains(t, plain, "PRIVATE_TOOL_RESULT_")
	}
	require.Zero(t, m.historyStart)
	require.True(t, observedWork, "ContinueHistory reaches Work sources without leaking individual child rows")
	m.scrollToBottom()
	m.ensureAllItemsRendered()
	full := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Equal(t, 1, strings.Count(full, "Work [+]"), "all source children remain in one human-bounded Work group")
	finalSource := len(m.messages) - 1
	_, inWork := m.workSourceToHeader[finalSource]
	require.False(t, inWork, "the latest ordinary response stays outside collapsed Work")
	require.Contains(t, ansi.Strip(m.View()), "FINAL_ANSWER_REMAINS_VISIBLE", "the live segmented tail remains visible beside Work")
	require.LessOrEqual(t, strings.Count(full, "PRIVATE_THOUGHT_"), 3, "collapsed Work shows only the newest reasoning preview")
	require.NotContains(t, full, "PRIVATE_INTERMEDIATE_REPLY_")
	require.NotContains(t, full, "PRIVATE_TOOL_RESULT_")
	require.Less(t, initialStart, len(m.messages))
	require.Len(t, m.messages, len(originalMessages))
	for i, source := range originalMessages {
		require.Same(t, source, m.messages[i], "source message pointer %d is not replaced or reordered", i)
		require.Same(t, originalViews[i], m.views[i], "source view pointer %d is not replaced", i)
	}
	require.True(t, reflect.DeepEqual(originalSession.Messages, sess.Messages), "persisted source transcript remains unchanged")

	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	block.SetSize(m.contentWidth()+1, 0) // settle the shared disclosure immediately for replay rendering
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	expanded := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, expanded, "PRIVATE_THOUGHT_00")
	require.Contains(t, expanded, "PRIVATE_INTERMEDIATE_REPLY_00")
	require.Equal(t, 40, m.MessageTypeCount(types.MessageTypeToolCall), "tool source views remain owned by the same chronological Work block")
	require.NotContains(t, expanded, "PRIVATE_TOOL_RESULT_00", "nested tool cards retain their own collapsed state until individually expanded")
	require.NotContains(t, expanded, "Earlier reply", "historical ordinary replies keep content without repeated headings")
}

func TestCompactTogglePreservesVisibleSourceAnchorAndSourceOwnership(t *testing.T) {
	sess := session.New(session.WithID("work-toggle-anchor"))
	sess.AddMessage(session.UserMessage("first human boundary"))
	for i := range 16 {
		call := tools.ToolCall{ID: fmt.Sprintf("anchor-call-%02d", i), Function: tools.FunctionCall{Name: "probe"}}
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{
			Role:      chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "probe"}},
			Presentation: []chat.AssistantPart{
				{Type: chat.AssistantPartReasoning, Text: fmt.Sprintf("ANCHOR_THOUGHT_%02d", i)},
				{Type: chat.AssistantPartContent, Text: fmt.Sprintf("ANCHOR_REPLY_%02d", i)},
				{Type: chat.AssistantPartToolCall, ToolCallID: call.ID},
			},
		}))
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: fmt.Sprintf("ANCHOR_TOOL_RESULT_%02d", i)}))
	}
	secondHuman := session.UserMessage("SECOND_HUMAN_BOUNDARY with enough wrapped text to span several rows\nline two\nline three\nline four")
	sess.AddMessage(secondHuman)
	for i := range 12 {
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: fmt.Sprintf("TAIL_REASONING_%02d", i), Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: fmt.Sprintf("TAIL_REASONING_%02d", i)}}}))
	}
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "TAIL_FINAL_ANSWER", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "TAIL_FINAL_ANSWER"}}}))

	ar := animation.NewRuntime()
	state := &service.SessionState{}
	state.SetExpandThinking(true)
	m := NewScrollableView(ar, 64, 8, state).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	finishAllHistoryForTest(t, m)
	m.ensureAllItemsRendered()
	anchorSource := -1
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeUser && msg.Content == secondHuman.Message.Content {
			anchorSource = i
			break
		}
	}
	require.GreaterOrEqual(t, anchorSource, 0)
	anchorGlobal := m.lineOffsets[anchorSource] + 2
	wantSource, wantLocal := m.globalLineToMessageLineCached(anchorGlobal)
	require.Equal(t, wantSource, anchorSource)
	require.Equal(t, 2, wantLocal)
	m.userHasScrolled = true
	m.setScrollOffset(anchorGlobal)
	_ = m.View()

	sourceMessages := append([]*types.Message(nil), m.messages...)
	sourceViews := append([]layout.Model(nil), m.views...)
	contents := make([]string, len(m.messages))
	for i := range m.messages {
		contents[i] = m.messages[i].Content
	}
	m.SetCompactWorkLog(true)
	gotSource, gotLocal := m.globalLineToMessageLineCached(m.scrollOffset)
	require.Equal(t, wantSource, gotSource, "flat-to-compact toggle retains the visible source anchor")
	require.Equal(t, wantLocal, gotLocal, "flat-to-compact toggle retains the child-local line")
	for i := range sourceMessages {
		require.Same(t, sourceMessages[i], m.messages[i])
		require.Same(t, sourceViews[i], m.views[i])
		require.Equal(t, contents[i], m.messages[i].Content)
	}

	m.SetCompactWorkLog(false)
	gotSource, gotLocal = m.globalLineToMessageLineCached(m.scrollOffset)
	require.Equal(t, wantSource, gotSource, "compact-to-flat toggle preserves the same human source")
	require.Equal(t, wantLocal, gotLocal, "compact-to-flat toggle restores the same local row")
	require.True(t, reflect.DeepEqual(contents, messageContents(m.messages)), "source text remains byte-for-byte lossless across both display toggles")
	for i := range sourceMessages {
		require.Same(t, sourceMessages[i], m.messages[i])
		require.Same(t, sourceViews[i], m.views[i])
	}
}

func finishAllHistoryForTest(t *testing.T, m *model) {
	t.Helper()
	for steps := 0; HistoryPending(m) && steps < 10000; steps++ {
		runReplayCommand(m, ContinueHistory(m))
	}
	require.False(t, HistoryPending(m), "all history pages are eventually included")
}

func messageContents(messages []*types.Message) []string {
	out := make([]string, len(messages))
	for i := range messages {
		out[i] = messages[i].Content
	}
	return out
}

type historyToggleClock struct{ now time.Time }

func (c *historyToggleClock) Now() time.Time { return c.now }
func (c *historyToggleClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(delay)
		return create(c.now)
	}
}
