package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActualProgramSessionTitleSingleHintDoubleRenameScopedClicks(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.hideSidebar = false
	root.initSessionComponents("profile", root.application, root.application.Session())
	root.chatPage.Init()
	root.resizeAll()
	model := &shellProgramModel{root: root}
	bounds := root.paneShell.Sidebar
	program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
	frame := sidebarProgramSnapshot(t, program)
	// The pane header repeats the session title. Target only the measured
	// sidebar rectangle, not the first textual occurrence in the full frame.
	x, y := -1, -1
	for row, line := range strings.Split(ansi.Strip(frame.content), "\n") {
		if row < bounds.Y || row >= bounds.Y+bounds.Height {
			continue
		}
		segment := ansi.Cut(line, bounds.X, bounds.X+bounds.Width)
		if prefix, _, ok := strings.Cut(segment, "profile"); ok {
			x, y = bounds.X+ansi.StringWidth(prefix)+2, row
			break
		}
	}
	require.GreaterOrEqual(t, x, 0)
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		s := sidebarProgramSnapshot(t, program)
		return strings.Contains(ansi.Strip(s.content), "Double-click to rename the session") && !s.open
	}, time.Second, time.Millisecond)
	require.Empty(t, sidebarProgramSnapshot(t, program).editor, "single click never enters composer")
	program.Send(tea.MouseClickMsg{X: 0, Y: y + 8, Button: tea.MouseLeft})
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Double-click to rename the session")
	}, time.Second, time.Millisecond, "unrelated click resets double-click scope")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(tea.KeyPressMsg{Code: 'Z', Text: "Z"})
	require.Eventually(t, func() bool {
		return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "profileZ")
	}, time.Second, time.Millisecond, "second scoped title click enters existing inline editor")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		return !strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "profileZ")
	}, time.Second, time.Millisecond)
	program.Send(messages.ShowInteractionHintMsg{SessionID: "foreign-session", Text: "MUST-NOT-SHOW"})
	require.NotContains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "MUST-NOT-SHOW")
}

type queueRemovalHandle struct {
	*lifecycleHandle

	removed chan string
}

func (h *queueRemovalHandle) CancelPendingMessage(_ context.Context, turnID string) (bool, error) {
	h.removed <- turnID
	return true, nil
}

type queueRemovalSessions struct {
	*openSubagentSessions

	handle *queueRemovalHandle
}

func (s *queueRemovalSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return s.handle, nil
}

func TestActualProgramQueueRemoveUsesExactCanonicalIDAndEvent(t *testing.T) {
	for _, clearHint := range []bool{true, false} {
		name := "cleared-hint"
		if !clearHint {
			name = "active-hint"
		}
		t.Run(name, func(t *testing.T) {
			sess := session.New(session.WithID("queue-actions-root"), session.WithAgentName("root"), session.WithTitle("QUEUE-ACTIONS"))
			handle := &queueRemovalHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, removed: make(chan string, 1)}
			application := app.New(t.Context(), &queueRemovalSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			root := newSidebarProgramRoot(t, application)
			root.editor.SetValue("KEEP-COMPOSER")
			model := &shellProgramModel{root: root}
			program := startTestProgram(t, root, model, tea.WithOutput(&cacheProgramWriter{}))
			program.Send(runtime.PendingUserMessageAccepted(sess.ID, "turn-first-full-ID", "First queued body", nil, 1))
			program.Send(runtime.PendingUserMessageAccepted(sess.ID, "turn-second-full-ID", "Second queued body", nil, 2))
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && strings.Contains(ansi.Strip(s.content), "Second queued body")
			}, time.Second, time.Millisecond)
			frame := sidebarProgramSnapshot(t, program)
			x, y := sidebarProgramPoint(t, frame.content, "Second queued body")
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Eventually(t, func() bool {
				return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "Double-click to edit queued message")
			}, time.Second, time.Millisecond)
			require.Equal(t, "KEEP-COMPOSER", sidebarProgramSnapshot(t, program).editor)
			if clearHint {
				program.Send(messages.ShowInteractionHintMsg{SessionID: sess.ID})
			}
			program.Send(tea.MouseMotionMsg{X: x, Y: y})
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return (!clearHint || s.active == 0) && strings.Contains(strings.Split(ansi.Strip(s.content), "\n")[y], "×")
			}, time.Second, time.Millisecond)
			if !clearHint {
				require.Contains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "Double-click to edit queued message", "pointer motion preserves the useful reminder until action or expiry")
			}
			row := strings.Split(ansi.Strip(sidebarProgramSnapshot(t, program).content), "\n")[y]
			index := strings.LastIndex(row, "×")
			removeX := ansi.StringWidth(row[:index])
			program.Send(tea.MouseClickMsg{X: removeX, Y: y, Button: tea.MouseLeft})
			select {
			case removed := <-handle.removed:
				require.Equal(t, "turn-second-full-ID", removed)
			case <-time.After(time.Second):
				t.Fatal("canonical withdrawal was not invoked")
			}
			require.Contains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "Second queued body", "no optimistic removal before canonical event")
			program.Send(runtime.PendingUserMessageCanceled(sess.ID, "turn-second-full-ID", 2))
			require.Eventually(t, func() bool {
				s := sidebarProgramSnapshot(t, program)
				return s.active == 0 && !strings.Contains(ansi.Strip(s.content), "Second queued body")
			}, time.Second, time.Millisecond)
			require.Contains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "First queued body")
			require.Equal(t, "KEEP-COMPOSER", sidebarProgramSnapshot(t, program).editor)
		})
	}
}
