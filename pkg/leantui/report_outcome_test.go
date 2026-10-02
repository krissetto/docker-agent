package leantui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestHistoricalReportOutcomeLiveAndReplay(t *testing.T) {
	for _, tc := range []struct {
		outcome session.ReportOutcome
		label   string
	}{{session.ReportOutcomeFinished, "turn finished"}, {session.ReportOutcomeFailed, "turn failed"}, {"", "report received"}} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			m := bareModel(40)
			sess := session.New(session.WithID("s"))
			input := session.UserMessage("private child report")
			input.InputOrigin, input.InputMode, input.SenderID, input.SenderName, input.TurnID, input.ReportOutcome = session.InputOriginRuntime, "steer", "child", "worker", "report", tc.outcome
			sess.AddMessage(input)
			m.handleEvent(t.Context(), &runtime.PendingUserMessagePromotedEvent{TurnID: input.TurnID, InputOrigin: input.InputOrigin, InputMode: input.InputMode, SenderID: input.SenderID, SenderName: input.SenderName, Message: input.Message.Content, ReportOutcome: tc.outcome})
			view := func() string {
				return ansi.Strip(strings.Join(m.screen.Transcript.Lines(100, 0, false, m.sessionState, nil), "\n"))
			}
			live := view()
			assert.Contains(t, live, tc.label)
			m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: "s"}, TranscriptPosition: 1}})
			assert.Equal(t, live, view())
		})
	}
}
