package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
)

// A notification lifted above typed input keeps its close glyph clickable at
// the moved position, and clicking it dismisses the stack.
func TestRootNotificationCloseClickDismissesLiftedStack(t *testing.T) {
	clock := &rootImmediateScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(clock))
	root.resizeAll()
	root.Update(notification.ShowMsg{Text: "Notification avoids typed input"})
	resting := root.notification.GetLayers()[0].GetY()

	root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.resizeAll()
	root.syncNotificationAvoidance()
	for range 80 {
		cmd := root.ar.Continue()
		if cmd == nil {
			break
		}
		root.updateWithLifecycle(cmd())
		root.syncNotificationAvoidance()
	}
	require.Less(t, root.notification.GetLayers()[0].GetY(), resting, "precondition: the stack is lifted")

	x, y, found := -1, -1, false
	for row := range root.height {
		for col := range root.width {
			if _, ok := root.notification.CloseButtonHit(col, row); ok {
				x, y, found = col, row, true
				break
			}
		}
		if found {
			break
		}
	}
	require.True(t, found, "the lifted close glyph is hittable on screen")

	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd, "the close click returns a dismiss command")
	for _, msg := range collectMsgs(cmd) {
		root.updateWithLifecycle(msg)
	}
	assert.False(t, root.notification.Open(), "clicking the lifted close glyph dismisses the stack")
}
