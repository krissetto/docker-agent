package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// agentInputItem is the parent's task as an async subagent receives it: a
// pending, accepted, agent-origin user message. The transcript shows it as the
// "From <parent> sent a message" receipt, and in an attached child it is the
// session opener.
func agentInputItem(sess *session.Session, text string) {
	input := session.UserMessage(text)
	input.InputOrigin = session.InputOriginAgent
	input.SenderID, input.SenderName = "parent-session", "orchestrator"
	sess.AddMessage(input)
}

// workPagedChild is the shape that exposed the bug: the parent's task, some
// turns, then a long final answer whose rows alone cover the viewport, so the
// first replay frame leaves the task below historyStart.
func workPagedChild(turns int) *session.Session {
	sess := session.New(session.WithID("work-paged-child"))
	agentInputItem(sess, "investigate the flaky test and report regressions")
	for i := range turns {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("follow-up %02d %s", i, strings.Repeat("h ", 25))))
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("reply %02d %s", i, strings.Repeat("r ", 25))}))
	}
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: strings.Repeat("final answer line.\n", 30)}))
	return sess
}

// A slot paged in from unpaged history must get a placeholder height: a slot at
// height zero owns no line, so the viewport scan can never enter it to measure
// it, and the message stays invisible no matter how the reader scrolls.
func TestWorkModePagedHistorySlotsGetPlaceholderHeight(t *testing.T) {
	for _, turns := range []int{0, 6, 14} {
		t.Run(fmt.Sprint(turns), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 100, 16, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			m.SetCompactWorkLog(true)
			sess := workPagedChild(turns)
			BeginReplay(m, PrepareReplay(sess), nil)
			finishPreparedReplay(t, m)
			unpaged := m.historyStart

			finishHistoryReplay(t, m)
			require.Zero(t, m.historyStart)
			require.False(t, HistoryPending(m))

			// Every paged-in slot that is not folded into a Work block must own
			// rows; a zero-height slot can never be measured.
			for i := range unpaged {
				if m.hiddenSlot(i) {
					continue
				}
				require.Positive(t, m.geom.Height(i), "slot %d was paged in at zero height and can never be measured", i)
			}

			m.scrollToTop()
			for range 3 {
				_ = m.View()
			}
			require.Contains(t, ansi.Strip(m.View()), "investigate the flaky test",
				"the parent's task must render at the top of an attached child transcript")
		})
	}
}

// The opener receipt must survive a geometry epoch: a resize or theme change
// after replay must not blank the row that paging made visible.
func TestWorkModePagedHistorySurvivesEpoch(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 100, 16, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	sess := workPagedChild(0)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	finishHistoryReplay(t, m)

	m.scrollToTop()
	require.Contains(t, ansi.Strip(m.View()), "investigate the flaky test")

	m.SetSize(90, 16)
	m.scrollToTop()
	for range 3 {
		_ = m.View()
	}
	require.Contains(t, ansi.Strip(m.View()), "investigate the flaky test",
		"a width epoch must not blank a paged-in history row")
}
