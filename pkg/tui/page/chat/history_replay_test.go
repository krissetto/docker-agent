package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestRecentReplayAcceptsLiveEventsAndResetWhilePrefixRenders(t *testing.T) {
	sess := session.New(session.WithID("recent-replay"))
	for i := range 60 {
		content := fmt.Sprintf("historical assistant %d", i)
		if i == 19 {
			content = strings.Repeat("oversized historical prefix\n\n", 1500)
		}
		sess.AddMessage(&session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: content}})
	}
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	defer ar.Stop()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	defer Cleanup(p)
	p.SetSize(120, 40)
	EnableAsyncReplay(p)
	prepared := p.beginReplay(runtime.SessionSnapshot{Session: sess, TranscriptPosition: sess.ItemCount()})()
	p.Update(prepared)
	p.Update(&runtime.UserMessageEvent{Message: "queued before first usable", SessionPosition: sess.ItemCount(), TurnID: "queued"})
	for range 100 {
		if !Loading(p) {
			break
		}
		p.Update(replayContinueMsg{generation: p.replayGeneration})
	}
	require.False(t, Loading(p))
	require.True(t, list.HistoryPending(p.messages))
	p.SetPresentationVisible(false)
	require.Nil(t, p.continueHistoryCmd(), "hidden pages pause prefix rendering")
	_, _, beforeHidden := p.messages.ResizeCacheStats()
	p.Update(historyContinueMsg{generation: p.replayGeneration})
	_, _, afterHidden := p.messages.ResizeCacheStats()
	require.Equal(t, beforeHidden, afterHidden, "a queued hidden-page turn must not render prefix bodies")
	p.SetPresentationVisible(true)
	require.NotNil(t, p.continueHistoryCmd(), "showing the page resumes retained prefix work")
	require.Contains(t, ansi.Strip(p.messages.View()), "queued before first usable")
	p.Update(&runtime.UserMessageEvent{Message: "live input while prefix warms", SessionPosition: sess.ItemCount() + 1, TurnID: "live"})
	require.Contains(t, ansi.Strip(p.messages.View()), "live input while prefix warms")
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeUser))

	// Capture the slow prefix's immutable worker without executing it yet.
	worker := list.ContinueHistory(p.messages)
	require.NotNil(t, worker)
	replacement := sess.Clone()
	replacement.AddMessage(session.UserMessage("authoritative replacement tail"))
	_, reset := p.Update(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: replacement, TranscriptPosition: replacement.ItemCount()}})
	require.True(t, Loading(p))
	// Route the reset's prepared snapshot (no routed id in this focused page).
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				run(child)
			}
		case replayPreparedMsg:
			p.Update(msg)
		}
	}
	run(reset)
	p.Update(worker())
	for range 100 {
		if !Loading(p) {
			break
		}
		p.Update(replayContinueMsg{generation: p.replayGeneration})
	}
	require.False(t, Loading(p))
	require.Contains(t, ansi.Strip(p.messages.View()), "authoritative replacement tail")
	require.NotContains(t, ansi.Strip(p.messages.View()), "live input while prefix warms")
}
