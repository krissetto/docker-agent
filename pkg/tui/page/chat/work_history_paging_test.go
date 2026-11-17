package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	list "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// unwrapPageCmd drains routed/owner wrappers and batch messages into the leaf
// messages the page's own command produced.
func unwrapPageCmd(msg tea.Msg) []tea.Msg {
	switch m := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, child := range m {
			if child != nil {
				out = append(out, unwrapPageCmd(child())...)
			}
		}
		return out
	case replayOwnerMsg:
		return unwrapPageCmd(m.inner)
	default:
		return []tea.Msg{m}
	}
}

// attachedChildSession is the shape that exposed the stall: the parent's task
// arrives as an agent-origin input, several turns follow, and a long final
// answer covers the viewport alone, so the first replay frame leaves the task
// below historyStart.
func attachedChildSession(turns int) *session.Session {
	sess := session.New(session.WithID("attached-child"))
	input := session.UserMessage("investigate the flaky test and report regressions")
	input.InputOrigin = session.InputOriginAgent
	input.SenderID, input.SenderName = "parent-session", "orchestrator"
	sess.AddMessage(input)
	for i := range turns {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("follow-up %02d %s", i, strings.Repeat("h ", 25))))
		sess.AddMessage(session.NewAgentMessage("worker", &chatmsg.Message{
			Role: chatmsg.MessageRoleAssistant, Content: fmt.Sprintf("reply %02d %s", i, strings.Repeat("r ", 25))}))
	}
	sess.AddMessage(session.NewAgentMessage("worker", &chatmsg.Message{
		Role: chatmsg.MessageRoleAssistant, Content: strings.Repeat("final answer line.\n", 30)}))
	return sess
}

// The page owns the history paging tick, so it must handle the continuation
// message ContinueHistory returns. Dropping it stalls paging with older history
// left unmeasured.
func TestPageReArmsHistoryPagingUntilHistoryIsDone(t *testing.T) {
	for _, work := range []bool{true, false} {
		t.Run(fmt.Sprintf("compact-work=%v", work), func(t *testing.T) {
			sess := attachedChildSession(6)
			a, _ := newSessionTestApp(t, sess, nil, nil)
			ar := animation.NewRuntime()
			defer ar.Stop()
			p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			defer Cleanup(p)
			p.SetSize(100, 16)
			p.SetCompactWorkLog(work)
			EnableAsyncReplay(p)

			p.Update(p.beginReplay(runtime.SessionSnapshot{Session: sess, TranscriptPosition: sess.ItemCount()})())
			for range 400 {
				if !Loading(p) {
					break
				}
				_, cmd := p.Update(replayContinueMsg{generation: p.replayGeneration})
				for _, m := range unwrapPageCmd(cmd()) {
					p.Update(m)
				}
			}
			require.False(t, Loading(p), "the replay must finish")
			require.True(t, list.HistoryPending(p.messages), "older history is left to page in")

			// Drive paging the way the program does: inject the first tick, then
			// only ever feed messages the page itself produced. A page that drops
			// the continuation message schedules no further tick, so paging stops
			// with older history still pending.
			queue := []tea.Msg{historyContinueMsg{generation: p.replayGeneration}}
			for range 500 {
				if len(queue) == 0 || !list.HistoryPending(p.messages) {
					break
				}
				_, cmd := p.Update(queue[0])
				queue = queue[1:]
				if cmd != nil {
					queue = append(queue, unwrapPageCmd(cmd())...)
				}
			}
			require.False(t, list.HistoryPending(p.messages), "history paging must reach the oldest item")
		})
	}
}
