package tui

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActualProgramLastTabCloseUsesGlobalExitConfirmation(t *testing.T) {
	for _, state := range []string{"idle", "running", "attached"} {
		t.Run(state, func(t *testing.T) {
			root, handle := responseTestRoot(t)
			root.editor.SetValue("SOLE-DRAFT")
			if state == "attached" {
				info := runtime.SubagentAttachInfo{NodeID: "sole-full-node", Session: root.application.Session(), Agent: "root", ParentSessionID: "closed-parent", ParentAgent: "parent"}
				root.application = newAttachedSubagentApp(t.Context(), root.application.SessionRuntime(), root.application.Runtime(), info, root.application.Binding())
				root.supervisor.ReplaceRunnerApp(t.Context(), root.application.Session().ID, SpawnedSession{App: root.application, Session: root.application.Session(), Ownership: RuntimeBorrowed}, "")
			}
			var cleanup atomic.Int32
			root.runCleanup = func() { cleanup.Add(1) }
			model := &shellProgramModel{root: root}
			program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
			if state == "running" {
				program.Send(runtime.StreamStarted("response-confirm-session", "root"))
			}
			initial := sidebarProgramSnapshot(t, program)
			plain := ansi.Strip(initial.tabView)
			index := strings.Index(plain, "×")
			require.GreaterOrEqual(t, index, 0)
			x, y := ansi.StringWidth(plain[:index])+tabFrameOrigin(), initial.tabY
			for _, cancel := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeyEscape}} {
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.open && strings.Contains(ansi.Strip(s.content), "Exit")
				}, time.Second, time.Millisecond)
				require.Zero(t, cleanup.Load())
				require.Zero(t, handle.cancels.Load())
				program.Send(cancel)
				require.Eventually(t, func() bool { return !sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
				s := sidebarProgramSnapshot(t, program)
				require.Len(t, s.tabIDs, 1)
				require.Equal(t, "SOLE-DRAFT", s.editor)
			}
			program.Send(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
			require.Eventually(t, func() bool { return sidebarProgramSnapshot(t, program).open }, time.Second, time.Millisecond)
			require.Zero(t, handle.cancels.Load())
			program.Send(tea.KeyPressMsg{Code: 'y', Text: "y"})
			require.Eventually(t, func() bool { return cleanup.Load() == 1 }, time.Second, time.Millisecond)
			require.Zero(t, handle.cancels.Load(), "tab intent routes globalshutdown, not response cancellation")
		})
	}
}

func TestSoleCloseActionDoesNotDetachBeforeExitDecision(t *testing.T) {
	root, handle := responseTestRoot(t)
	id := root.supervisor.ActiveID()
	_, cmd := root.Update(messages.CloseTabMsg{SessionID: id})
	applyOpenDialogMsgs(t, root, cmd)
	require.True(t, root.dialogMgr.TopIsExitConfirmation())
	require.Equal(t, id, root.supervisor.ActiveID())
	require.NotNil(t, root.supervisor.GetRunner(id))
	require.Zero(t, handle.cancels.Load())
}
