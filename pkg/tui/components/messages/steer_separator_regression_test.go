package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// A steer appended after a Work child keeps the preceding visible reply's
// separator on the first render, and through the working/processing transition.
func TestSteerAfterWorkChildKeepsPriorReplySeparatorOnFirstRender(t *testing.T) {
	m := newAttachTestModel(t)
	m.SetSize(80, 40)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("first request")
	m.AppendReasoning("root", "planning")
	m.AppendToLastMessage("root", "visible reply")
	m.AppendReasoning("root", "second thought")
	m.View()

	steer := session.UserMessage("steer please")
	steer.TurnID = "t1"
	m.AddInputMessage(steer, -1)

	assertSteerSpacing := func(stage string) {
		t.Helper()
		rows := strings.Split(ansi.Strip(m.View()), "\n")
		row := -1
		for i, r := range rows {
			if strings.Contains(r, "steer please") {
				row = i
				break
			}
		}
		require.Greater(t, row, 0, stage)
		require.Equal(t, "┃", strings.TrimSpace(rows[row-1]), stage+": steer's top border")
		require.Empty(t, strings.TrimSpace(rows[row-2]), stage+": blank separator under the visible reply")
		require.Contains(t, rows[row-3], "visible reply", stage)
	}

	assertSteerSpacing("first pending frame")
	m.SetWorkStatus(true, 0)
	assertSteerSpacing("working section active")
	m.AddAssistantMessage("root", "")
	assertSteerSpacing("processing spinner at tail")
	m.SetWorkStatus(false, 0)
	m.RemoveSpinner()
	assertSteerSpacing("settled")
}
