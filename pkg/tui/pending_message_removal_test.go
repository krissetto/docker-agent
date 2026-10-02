package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func pendingRemovalRoot(t *testing.T) (*appModel, *queueRemovalHandle) {
	t.Helper()
	sess := session.New(session.WithID("removal-owner"))
	handle := &queueRemovalHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, removed: make(chan string, 2)}
	application := app.New(t.Context(), &queueRemovalSessions{openSubagentSessions: &openSubagentSessions{}, handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	return newSidebarProgramRoot(t, application), handle
}

func removalConfirmation(t *testing.T, root *appModel) pendingRemovalConfirmedMsg {
	t.Helper()
	_, _ = root.Update(messages.OpenPendingRemovalMsg{SessionID: root.application.Session().ID, TurnID: "exact-turn"})
	require.True(t, root.dialogMgr.Open())
	_, cmd := root.dialogMgr.TopDialog().Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	for _, msg := range collectMsgs(cmd) {
		if confirmed, ok := msg.(pendingRemovalConfirmedMsg); ok {
			return confirmed
		}
	}
	t.Fatal("missing confirmation")
	return pendingRemovalConfirmedMsg{}
}

func TestPendingRemovalRejectsChangedSessionAndRoute(t *testing.T) {
	for _, change := range []string{"app", "session-in-same-app", "route-round-trip", "cleanup"} {
		t.Run(change, func(t *testing.T) {
			root, handle := pendingRemovalRoot(t)
			confirmed := removalConfirmation(t, root)
			switch change {
			case "app":
				root.application = app.New(t.Context(), nil, session.New(session.WithID("different")), runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			case "session-in-same-app":
				root.application.NewSession()
			case "route-round-trip":
				root.pendingRemovalGeneration++
			case "cleanup":
				root.contextClosed = true
			}
			_, cmd := root.Update(confirmed)
			msg, ok := cmd().(notification.ShowMsg)
			require.True(t, ok)
			require.Contains(t, msg.Text, "session changed")
			select {
			case <-handle.removed:
				t.Fatal("stale confirmation reached runtime")
			default:
			}
		})
	}
}

func TestPendingRemovalWorkerPinsOwnerAndReportsCanonicalOutcome(t *testing.T) {
	for _, outcome := range []string{"removed", "promoted-or-deleted", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			root, handle := pendingRemovalRoot(t)
			handle.notPending = outcome == "promoted-or-deleted"
			if outcome == "failure" {
				handle.failure = errors.New("durable deletion failed")
			}
			_, cmd := root.Update(removalConfirmation(t, root))
			// A route change after command dispatch must not retarget the worker.
			root.application.NewSession()
			result := cmd()
			require.Equal(t, "exact-turn", <-handle.removed)
			switch outcome {
			case "removed":
				require.Nil(t, result)
			case "promoted-or-deleted":
				require.Equal(t, notification.ShowMsg{Text: "Queued message is no longer pending", Type: notification.TypeInfo}, result)
			case "failure":
				require.Equal(t, notification.ShowMsg{Text: "Failed to remove queued message: durable deletion failed", Type: notification.TypeError}, result)
			}
		})
	}
}

func TestPendingRemovalKeyboardCancelNeverReachesRuntime(t *testing.T) {
	for _, code := range []rune{tea.KeyEscape, tea.KeyEnter, 'n'} {
		root, handle := pendingRemovalRoot(t)
		_, _ = root.Update(messages.OpenPendingRemovalMsg{SessionID: handle.ID(), TurnID: "exact-turn"})
		_, cmd := root.dialogMgr.TopDialog().Update(tea.KeyPressMsg{Code: code})
		for _, msg := range collectMsgs(cmd) {
			root.Update(msg)
		}
		select {
		case <-handle.removed:
			t.Fatal("cancellation reached runtime")
		default:
		}
	}
}

func TestPendingRemovalActualSessionRoundTripInvalidatesConfirmation(t *testing.T) {
	root := splitTestRoot(t)
	confirmed := removalConfirmation(t, root)
	original := root.application
	root.handleSwitchTab("second")
	root.handleSwitchTab("profile")
	require.Same(t, original, root.application)
	require.Greater(t, root.pendingRemovalGeneration, confirmed.generation)
	_, cmd := root.Update(confirmed)
	require.Contains(t, cmd().(notification.ShowMsg).Text, "session changed")
}

func TestPendingRemovalDisplaysRequestedSnapshot(t *testing.T) {
	root, handle := pendingRemovalRoot(t)
	request := messages.OpenPendingRemovalMsg{SessionID: handle.ID(), TurnID: "exact-turn", Content: "Chosen queued text λ界\nsecond line"}
	_, _ = root.Update(request)
	require.True(t, root.dialogMgr.Open())
	request.Content = "Changed later"
	d := root.dialogMgr.TopDialog()
	d.SetSize(120, 40)
	view := ansi.Strip(d.View())
	require.Contains(t, view, "Chosen queued text λ界")
	require.Contains(t, view, "second line")
	require.NotContains(t, view, request.Content)
	require.NotContains(t, view, "Remove this queued message?")
}

func TestPendingRemovalRejectsWrongSnapshotOwner(t *testing.T) {
	root, _ := pendingRemovalRoot(t)
	_, _ = root.Update(messages.OpenPendingRemovalMsg{SessionID: "stale-owner", TurnID: "exact-turn", Content: "Wrong session text"})
	require.False(t, root.dialogMgr.Open())
}
