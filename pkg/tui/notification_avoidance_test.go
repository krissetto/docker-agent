package tui

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestRootNotificationAvoidanceBothCompositorsAndHitGeometry(t *testing.T) {
	for _, lean := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "lean"}[lean], func(t *testing.T) {
			clock := &rootImmediateScheduler{now: time.Unix(1, 0)}
			root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(clock))
			root.leanMode = lean
			root.panelSettings = messages.PanelSettings{Elements: []messages.PanelElement{}}
			root.resizeAll()
			root.Update(notification.ShowMsg{Text: "Notification avoids typed input"})
			resting := root.notification.GetLayer().GetY()
			root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
			root.focusedPanel = PanelEditor
			root.editor.Focus()
			root.resizeAll()
			root.syncNotificationAvoidance()
			require.True(t, root.ar.HasActive(), "overlap registers shared motion")
			for range 80 {
				cmd := root.ar.Continue()
				if cmd == nil {
					break
				}
				root.updateWithLifecycle(cmd())
				root.syncNotificationAvoidance()
			}
			layer := root.notification.GetLayer()
			require.Less(t, layer.GetY(), resting)
			frame := root.editorFrame()
			origin := image.Pt(frame.GetMarginLeft()+frame.GetBorderLeftSize()+frame.GetPaddingLeft(), root.editorTop()+frame.GetMarginTop()+frame.GetBorderTopSize()+frame.GetPaddingTop())
			bounds := image.Rect(layer.GetX(), layer.GetY(), layer.GetX()+layer.Width(), layer.GetY()+layer.Height())
			for _, cell := range root.editor.(editor.TextOccupancy).OccupiedTextCells() {
				require.False(t, bounds.Overlaps(cell.Add(origin)), "shifted overlay clears visible Unicode input")
			}
			require.Contains(t, root.View().Content, "Notification avoids typed input")
			root.notification.HandleMouseMotion(layer.GetX()+2, layer.GetY()+1)
			_, _, hit := root.notification.CopyHit(layer.GetX()+2, layer.GetY()+1)
			require.True(t, hit, "copy geometry moves with the painted stack")
			root.editor.SetValue("")
			root.syncNotificationAvoidance()
			for range 80 {
				cmd := root.ar.Continue()
				if cmd == nil {
					break
				}
				root.updateWithLifecycle(cmd())
				root.syncNotificationAvoidance()
			}
			require.Equal(t, resting, root.notification.GetLayer().GetY())
			require.False(t, root.ar.HasActive(), "settled empty editor leaves no avoidance tick")
			root.notification.Cleanup()
		})
	}
}

func TestRootNotificationAvoidanceIgnoresLoadingEmptyAndUnfocused(t *testing.T) {
	for _, mode := range []string{"loading", "empty", "unfocused", "zero-size"} {
		t.Run(mode, func(t *testing.T) {
			root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)}))
			root.notification.Update(notification.ShowMsg{Text: "No movement without visible focused text"})
			root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
			root.editor.Focus()
			root.resizeAll()
			root.focusedPanel = PanelEditor
			switch mode {
			case "loading":
				root.ready = false
			case "empty":
				root.editor.SetValue("")
			case "unfocused":
				root.focusedPanel = PanelContent
				root.editor.Blur()
			case "zero-size":
				root.width = 0
				root.height = 0
			}
			resting := root.notification.GetLayer().GetY()
			root.syncNotificationAvoidance()
			require.Equal(t, resting, root.notification.GetLayer().GetY())
			require.False(t, root.ar.HasActive(), "invisible occupancy cannot register motion")
			root.notification.Cleanup()
		})
	}
}
