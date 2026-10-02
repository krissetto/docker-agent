package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
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
