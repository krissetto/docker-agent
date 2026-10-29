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

func TestRuntimeNoticeMetadataImprovesSummaryAndWrapsAtToolGutter(t *testing.T) {
	for _, tc := range []struct {
		outcome session.ReportOutcome
		want    string
	}{{session.ReportOutcomeFinished, "Subagent turn finished"}, {session.ReportOutcomeFailed, "Subagent turn failed"}} {
		input := session.UserMessage("private report")
		input.InputOrigin, input.ReportOutcome = session.InputOriginRuntime, tc.outcome
		rendered := ansi.Strip(RenderInput(types.Input(input), 80))
		assert.Contains(t, rendered, tc.want)
		assert.NotContains(t, rendered, "private report")
		assert.True(t, strings.HasPrefix(rendered, "  "), "notice icon aligns with tool rows")
	}
	input := session.UserMessage(`<system_info>Subagent "worker" (child) finished its turn. Full response: "a long response that will wrap over a narrow terminal display"</system_info>`)
	input.InputOrigin, input.InputMode, input.SenderID, input.SenderName, input.ReportOutcome = session.InputOriginRuntime, "steer", "child-session", "worker", session.ReportOutcomeFinished
	lines := strings.Split(ansi.Strip(RenderInput(types.Input(input), 24)), "\n")
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 24)
	}
	if assert.Greater(t, len(lines), 1) {
		assert.True(t, strings.HasPrefix(lines[1], "  "), "wrapped detail continues at the tool gutter")
	}
}
