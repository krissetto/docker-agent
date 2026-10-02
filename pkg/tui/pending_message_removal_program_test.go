package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestActualProgramQueueConfirmationSurvivesDuplicateContentAndQueueMutation(t *testing.T) {
	for _, mutation := range []string{"other-deleted", "target-deleted", "target-promoted"} {
		t.Run(mutation, func(t *testing.T) {
			root, handle := pendingRemovalRoot(t)
			handle.notPending = mutation != "other-deleted"
			sessionID := root.application.Session().ID
			program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
			for i, id := range []string{"first-duplicate", "second-duplicate"} {
				program.Send(runtime.PendingUserMessageAccepted(sessionID, id, "Duplicate queued text", nil, i))
			}
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && strings.Count(ansi.Strip(s.content), "Duplicate queued text") == 2
			}, time.Second, time.Millisecond)
			frame := sidebarProgramSnapshot(t, program)
			x, y := -1, -1
			for row, line := range strings.Split(ansi.Strip(frame.content), "\n") {
				if prefix, _, found := strings.Cut(line, "Duplicate queued text"); found {
					x, y = ansi.StringWidth(prefix), row
				}
			}
			require.NotEqual(t, -1, x)
			program.Send(tea.MouseMotionMsg{X: x, Y: y})
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && strings.Contains(strings.Split(ansi.Strip(s.content), "\n")[y], "×")
			}, time.Second, time.Millisecond)
			row := strings.Split(ansi.Strip(sidebarProgramSnapshot(t, program).content), "\n")[y]
			index := strings.LastIndex(row, "×")
			program.Send(tea.MouseClickMsg{X: ansi.StringWidth(row[:index]), Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			switch mutation {
			case "other-deleted":
				program.Send(runtime.PendingUserMessageCanceled(sessionID, "first-duplicate", 0))
			case "target-deleted":
				program.Send(runtime.PendingUserMessageCanceled(sessionID, "second-duplicate", 1))
			case "target-promoted":
				program.Send(runtime.PendingUserMessagePromoted(sessionID, "second-duplicate", "Duplicate queued text", nil, 1))
			}
			_ = sidebarProgramSnapshot(t, program)
			program.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
			select {
			case id := <-handle.removed:
				require.Equal(t, "second-duplicate", id, "never remove the new occupant of a queue index or equal-content row")
			case <-time.After(time.Second):
				t.Fatal("missing canonical removal attempt")
			}
			require.Eventually(t, func() bool { return !sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			if mutation != "other-deleted" {
				require.Eventually(t, func() bool {
					return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Queued message is no longer pending")
				}, time.Second, time.Millisecond)
			}
		})
	}
}
