package chat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestHistoricalReportOutcomeLiveAndReplay(t *testing.T) {
	for _, tc := range []struct {
		outcome session.ReportOutcome
		label   string
	}{{session.ReportOutcomeFinished, "turn finished"}, {session.ReportOutcomeFailed, "turn failed"}, {"", "report received"}} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			sess := session.New(session.WithID("s"))
			a, _ := newSessionTestApp(t, sess, nil, nil)
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			p.messages.SetSize(100, 40)
			input := session.UserMessage("private child report")
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName, input.TurnID, input.ReportOutcome = session.InputOriginRuntime, "steer", "child", "worker", "report", tc.outcome
			sess.AddMessage(input)
			p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{TurnID: input.TurnID, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, Message: input.Message.Content, ReportOutcome: tc.outcome})
			live := ansi.Strip(p.messages.View())
			assert.Contains(t, live, tc.label)
			p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, TranscriptPosition: 1})
			assert.Equal(t, live, ansi.Strip(p.messages.View()))
		})
	}
}

func TestExplicitAgentMessagesAndCompletionStayDistinctLiveAndReplay(t *testing.T) {
	sess := session.New(session.WithID("parent"))
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "36c92-full", SessionID: "child", Agent: "engineer"}}}}
	sess.SetSubagentTree(&tree)
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.messages.SetSize(120, 40)
	p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID}})
	p.handleRuntimeEvent(&runtime.SubagentTreeEvent{Snapshot: tree})
	var events []*runtime.PendingUserMessagePromotedEvent
	for i := range 4 {
		input := session.UserMessage("same body")
		input.TurnID, input.InputOrigin, input.InputMode = fmt.Sprintf("explicit-%d", i), session.InputOriginAgent, "steer"
		input.SenderID, input.SenderName = "36c92-full", "engineer"
		if i == 0 {
			input.TurnID, input.InputOrigin, input.SenderID = "report:child-turn", session.InputOriginRuntime, "child"
			input.ReportOutcome = session.ReportOutcomeFinished
		} else if i == 2 {
			input.InputMode = "turn"
		}
		accepted := &runtime.PendingUserMessageAcceptedEvent{TurnID: input.TurnID, Message: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, ReportOutcome: input.ReportOutcome, SessionPosition: i}
		promoted := &runtime.PendingUserMessagePromotedEvent{TurnID: input.TurnID, Message: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, ReportOutcome: input.ReportOutcome, SessionPosition: i}
		p.handleRuntimeEvent(accepted)
		p.handleRuntimeEvent(accepted)
		p.handleRuntimeEvent(promoted)
		p.handleRuntimeEvent(promoted)
		p.handleRuntimeEvent(&runtime.UserMessageEvent{TurnID: input.TurnID, Message: input.Message.Content, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, ReportOutcome: input.ReportOutcome, SessionPosition: i})
		sess.AddMessage(input)
		events = append(events, promoted)
	}
	check := func() {
		t.Helper()
		out := ansi.Strip(p.messages.View())
		assert.Equal(t, 1, strings.Count(out, "engineer (36c92) · turn finished >"))
		assert.Equal(t, 3, strings.Count(out, "engineer (36c92) sent a message >"))
		assert.NotContains(t, out, "has replied")
		assert.NotContains(t, out, "same body")
		assert.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeRuntimeNotice))
		assert.Equal(t, 3, p.messages.MessageTypeCount(types.MessageTypeAgentInput))
		assert.Zero(t, p.messages.MessageTypeCount(types.MessageTypeAssistant))
		assert.Empty(t, p.messageQueue)
	}
	check()
	p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID}, TranscriptPosition: sess.ItemCount()})
	for _, event := range events {
		p.handleRuntimeEvent(event)
	}
	check()
}
