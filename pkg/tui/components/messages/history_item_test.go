package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestReplaceHistoryItemsKeepsWorkAndReaderAnchor(t *testing.T) {
	ar := animation.NewRuntime()
	renderers := tool.NewRegistry()
	renderers.Register("check", func(_ *animation.Runtime, msg *types.Message, _ service.SessionStateReader) layout.Model {
		return &labelView{label: "tool-card: " + msg.Content[:min(24, len(msg.Content))]}
	})
	m := NewScrollableView(ar, 90, 10, &service.SessionState{}, WithToolRenderers(renderers)).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	placeholder := session.Item{Error: &session.Error{Code: lifecycle.HistoryPlaceholderCode, Message: "Loading large transcript item…"}}
	call := tools.ToolCall{ID: "check-1", Function: tools.FunctionCall{Name: "check", Arguments: `{}`}}
	prefix := session.New(session.WithID("s"))
	prefix.Messages = []session.Item{placeholder}
	prefix.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "left thinking", ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "check"}}}))
	tail := session.New(session.WithID("s"))
	tail.Messages = []session.Item{placeholder}
	tail.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "right thinking", Content: strings.Repeat("reader anchor follows the restored tool\n\n", 36)}))
	BeginHistoryReplay(m, PrepareReplay(tail), nil, 2)
	finishPreparedReplay(t, m)
	finishHistoryReplay(t, m)
	results := make(map[string]string)
	PrependHistory(m, prefix, 0, results, nil)
	m.scrollToTop()
	require.Contains(t, ansi.Strip(m.View()), "Loading large transcript item")
	require.Equal(t, 2, m.MessageTypeCount(types.MessageTypeHistoryPlaceholder))
	require.Zero(t, m.MessageTypeCount(types.MessageTypeError))
	m.userHasScrolled = true
	m.setScrollOffset(m.slotOffset(len(m.views)-1) + 5)
	before := strings.Split(ansi.Strip(m.View()), "\n")
	beforeSticky := m.stickyWorkInset()
	toolBody := "FULL_RESULT_BEGIN " + strings.Repeat("result payload\n", 20_000) + "FULL_RESULT_END"
	result := session.New(session.WithID("s"))
	result.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: toolBody}))
	ReplaceHistoryItem(m, 2, result, results, nil, "")
	user := session.New(session.WithID("s"))
	attachment := strings.Repeat("document contents\n", 18_000) + "FULL_ATTACHMENT_END"
	user.AddMessage(session.UserMessage("Recovered user question", chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "large-notes.txt", MimeType: "text/plain", Source: chat.DocumentSource{InlineText: attachment}}}))
	ReplaceHistoryItem(m, 0, user, results, nil, "")
	after := strings.Split(ansi.Strip(m.View()), "\n")
	require.Contains(t, after[0], "Work [+]")
	after = after[m.stickyWorkInset():]
	before = before[beforeSticky:]
	for i := range min(len(before), len(after)) {
		require.Equal(t, before[i][:m.contentWidth()], after[i][:m.contentWidth()], "reader body row %d", i)
	}
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	require.Zero(t, m.MessageTypeCount(types.MessageTypeHistoryPlaceholder))
	require.Equal(t, attachment, m.messages[0].Attachments[0].Text)
	block := m.views[1].(*reasoningblock.Model)
	require.Equal(t, "left thinking\n\nright thinking", block.Reasoning())
	block.ToolCards(func(msg *types.Message, _ layout.Model) { require.Equal(t, toolBody, msg.Content) })
	m.SetCompactWorkLog(false)
	m.SetSize(90, 22)
	m.scrollToTop()
	block.Toggle()
	m.invalidateItem(1)
	frame := ansi.Strip(m.View())
	for _, text := range []string{"Recovered user question", "large-notes.txt", "tool-card: FULL_RESULT_BEGIN"} {
		require.Contains(t, frame, text)
	}
}
