package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestOlderHistoryKeepsTailLiveBoundary(t *testing.T) {
	tail := session.New(session.WithID("s"))
	tail.AddMessage(session.UserMessage("tail question"))
	answer := session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "tail answer"})
	tail.AddMessage(answer)
	a, _ := newSessionTestApp(t, tail, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(tail), WithCompactWorkLog(false)).(*chatPage)
	p.messages.SetSize(100, 100)
	t.Cleanup(func() { p.messages.StopAnimations(); ar.Stop() })
	p.updateReplay(p.resetProjection(runtime.SessionSnapshot{Session: tail, Epoch: "epoch", Cursor: 7, TranscriptPosition: 4, History: &session.BoundedItemPage{StartPosition: 2, BeforePosition: 4, TotalItems: 4, HasMore: true}, HistoryCursor: "previous"})())
	live := session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "live answer"})
	for _, event := range []runtime.Event{runtime.MessageAddedAt("s", answer, "root", 3), runtime.AgentChoice("root", "s", "live answer"), runtime.MessageAddedAt("s", live, "root", 4)} {
		handled, _ := p.updateReplay(event)
		require.True(t, handled)
	}
	for range 100 {
		if p.replay == nil {
			break
		}
		p.updateReplay(replayContinueMsg{generation: p.replayGeneration})
	}
	require.Nil(t, p.replay)
	require.Equal(t, 4, p.snapshotEnd)
	require.NotContains(t, ansi.Strip(p.messages.View()), "older question")
	require.NotNil(t, p.olderHistory)
	prefix := session.New(session.WithID("s"))
	prefix.AddMessage(session.UserMessage("older question"))
	prefix.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "older answer"}))
	p.updateReplay(olderHistoryPageMsg{history: p.olderHistory, prefix: prefix, page: runtime.SessionSnapshot{Epoch: "epoch", Cursor: 7, TranscriptPosition: 4, History: &session.BoundedItemPage{StartPosition: 0, BeforePosition: 2, TotalItems: 4, ScannedPositions: 2}}})
	p.messages.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	frame := ansi.Strip(p.messages.View())
	last := -1
	for _, text := range []string{"older question", "older answer", "tail question", "tail answer", "live answer"} {
		require.Equal(t, 1, strings.Count(frame, text), "%q in frame:\n%s", text, frame)
		pos := strings.Index(frame, text)
		require.Greater(t, pos, last)
		last = pos
	}
	require.True(t, list.MatchesUserMessage(p.messages, 0, 0, "older question"))
	require.True(t, list.MatchesUserMessage(p.messages, 2, 2, "tail question"))
}
