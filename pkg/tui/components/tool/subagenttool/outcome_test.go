package subagenttool

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestHistoricalCompletionOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome     session.ReportOutcome
		icon, label string
	}{
		{session.ReportOutcomeFinished, "✓", "turn finished"},
		{session.ReportOutcomeFailed, "✗", "turn failed"},
		{"", "·", "report received"},
	} {
		input := session.UserMessage("Subagent claims success, but text is not provenance")
		input.InputOrigin, input.SenderID, input.SenderName, input.ReportOutcome = session.InputOriginRuntime, "child", "worker", tc.outcome
		rendered := ansi.Strip(RenderInput(types.Input(input), 100))
		assert.Contains(t, rendered, tc.icon)
		assert.Contains(t, rendered, tc.label)
		assert.NotContains(t, rendered, "claims success")
	}
}

func TestSubagentToolErrorsExposeBoundedExplanationLiveAndReplay(t *testing.T) {
	for _, render := range []renderFunc{renderSpawn, renderSend, renderRead, renderStop} {
		for _, replay := range []bool{false, true} {
			msg := testMessage(subagent.ToolSendMessage, `{"to":"parent"}`, "delegation disabled; complete directly. "+strings.Repeat("detail ", 1000), types.ToolStatusError)
			if replay {
				msg.Content, msg.ToolResult = msg.ToolResult.Output, nil
			}
			out := ansi.Strip(renderer(render, nil)(msg, testSpinner(), nil, 80, 0))
			assert.Contains(t, out, "delegation disabled; complete directly.")
			assert.LessOrEqual(t, len(strings.Split(out, "\n")), 4)
			msg.ToolStatus = types.ToolStatusCompleted
			out = ansi.Strip(renderer(render, nil)(msg, testSpinner(), nil, 80, 0))
			assert.NotContains(t, out, "complete directly")
		}
	}
}
