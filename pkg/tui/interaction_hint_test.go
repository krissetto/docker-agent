package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestInteractionHintWaitsForBothDeadlines(t *testing.T) {
	root := splitTestRoot(t)
	started := time.Now()
	cmd := root.showInteractionHint(messages.ShowInteractionHintMsg{SessionID: root.application.Session().ID, Text: "delayed reminder"})
	require.False(t, root.interactionHintVisible)
	var ready interactionHintReadyMsg
	for _, msg := range collectMsgs(cmd) {
		if value, ok := msg.(interactionHintReadyMsg); ok {
			ready = value
		}
	}
	require.GreaterOrEqual(t, time.Since(started), max(styles.DoubleClickThreshold, 500*time.Millisecond))
	root.finishInteractionHint(ready)
	require.True(t, root.interactionHintVisible)
}

func TestInteractionHintStaleTimerCancellation(t *testing.T) {
	for _, action := range []string{"success", "key", "pointer", "focus", "modal", "replacement"} {
		t.Run(action, func(t *testing.T) {
			root := splitTestRoot(t)
			root.showInteractionHint(messages.ShowInteractionHintMsg{SessionID: root.application.Session().ID, Text: "old"})
			generation, _ := root.supervisor.RouteGeneration("profile")
			ready := interactionHintReadyMsg{generation: root.interactionHintGeneration, owner: "profile", routeGeneration: generation, sessionID: root.application.Session().ID, text: "old"}
			switch action {
			case "success":
				root.showInteractionHint(messages.ShowInteractionHintMsg{SessionID: root.application.Session().ID})
			case "key":
				root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			case "pointer":
				root.Update(tea.MouseMotionMsg{X: 1, Y: 1})
			case "focus":
				root.handleSwitchTab("second")
			case "modal":
				root.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "hint"}})
			case "replacement":
				root.showInteractionHint(messages.ShowInteractionHintMsg{SessionID: root.application.Session().ID, Text: "new"})
			}
			require.Nil(t, root.finishInteractionHint(ready))
			require.False(t, root.interactionHintVisible)
		})
	}
}
