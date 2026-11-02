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
		if i == 59 {
			// The oversized reply is the visible suffix, so its PrepareRender worker is real async work.
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
	// This test checks live events and reset ownership, not compact grouping;
	// compact-on replay is covered by the messages work tests.
	p.SetCompactWorkLog(false)
	EnableAsyncReplay(p)
	prepared := p.beginReplay(runtime.SessionSnapshot{Session: sess, TranscriptPosition: sess.ItemCount()})()
	p.Update(prepared)
	p.Update(&runtime.UserMessageEvent{Message: "queued before first usable", SessionPosition: sess.ItemCount(), TurnID: "queued"})
	// Each command the page returns while the replay waits is run through the page's own message flow.
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
		case list.ReplayRenderedMsg:
			p.Update(msg)
		}
	}
	for range 100 {
		if !Loading(p) {
			break
		}
		_, cmd := p.Update(replayContinueMsg{generation: p.replayGeneration})
		if list.ReplayWaiting(p.messages) {
			run(cmd)
		}
	}
	require.False(t, Loading(p))
	require.True(t, list.HistoryPending(p.messages))
	_, _, beforePrefix := p.messages.ResizeCacheStats()
	require.Nil(t, list.ContinueHistory(p.messages), "prefix paging is synchronous and schedules no async worker")
	_, _, afterPrefix := p.messages.ResizeCacheStats()
	require.Equal(t, beforePrefix, afterPrefix, "paging the prefix renders no body")
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

	// Two independent invariants are checked from here on: (a) live events and a reset are
	// accepted by the page, and (b) the real prepare worker captured from the page flow before the
	// second reset is executed and applied only after it. That late result belongs to the superseded
	// replay, so it is ignored: the replacement tail stays and the old live input stays absent.
	// A reset starts a replay whose visible oversized reply is in flight again.
	// That reply's PrepareRender command is captured but not run yet.
	first := sess.Clone()
	_, reset := p.Update(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: first, TranscriptPosition: first.ItemCount()}})
	require.True(t, Loading(p))
	run(reset)
	var stale tea.Cmd
	for range 100 {
		if !Loading(p) {
			break
		}
		_, cmd := p.Update(replayContinueMsg{generation: p.replayGeneration})
		if list.ReplayWaiting(p.messages) {
			stale = cmd
			break
		}
	}
	require.NotNil(t, stale, "the visible oversized reply must schedule a real PrepareRender command")
	require.True(t, list.ReplayWaiting(p.messages))

	// Reset again before that command runs. Its result belongs to the superseded replay.
	replacement := sess.Clone()
	replacement.AddMessage(session.UserMessage("authoritative replacement tail"))
	_, reset = p.Update(&app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: replacement, TranscriptPosition: replacement.ItemCount()}})
	require.True(t, Loading(p))
	run(reset)
	for range 100 {
		if !Loading(p) {
			break
		}
		_, cmd := p.Update(replayContinueMsg{generation: p.replayGeneration})
		run(cmd)
	}
	require.False(t, Loading(p))
	run(stale)
	require.Contains(t, ansi.Strip(p.messages.View()), "authoritative replacement tail")
	require.NotContains(t, ansi.Strip(p.messages.View()), "queued before first usable")
	require.NotContains(t, ansi.Strip(p.messages.View()), "live input while prefix warms")
}
