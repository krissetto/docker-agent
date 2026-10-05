package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestDelayedWorkingNotificationCannotRestartSettledSpinner(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.workingSpinner.Stop()
	require.False(t, root.chatPage.IsWorking())
	root.handleWorkingStateChanged(messages.WorkingStateChangedMsg{Working: false})
	root.handleWorkingStateChanged(messages.WorkingStateChangedMsg{Working: true})
	require.False(t, root.ar.HasActive(), "late start notification cannot lease a spinner after page stops")
}

func TestDelayedStoppedNotificationCannotStopActiveSpinner(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.chatPage.Update(runtime.StreamStarted(root.application.Session().ID, "root"))
	require.True(t, root.chatPage.IsWorking())
	root.handleWorkingStateChanged(messages.WorkingStateChangedMsg{Working: false})
	require.True(t, root.ar.HasActive(), "current page still owns active work")
}
