package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestMessageCopyHoverClearsOnComposerPointerLeave(t *testing.T) {
	for _, tc := range []struct {
		name     string
		align    bool
		position messages.SidebarPosition
	}{
		{name: "default", position: messages.SidebarRight},
		{name: "aligned-left", align: true, position: messages.SidebarLeft},
		{name: "aligned-right", align: true, position: messages.SidebarRight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := session.New(session.WithID("message-hover-" + tc.name))
			application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
			root := newSidebarProgramRoot(t, application)
			root.layoutSettings.AlignInputWithMessages = tc.align
			root.layoutSettings.SidebarPosition = tc.position
			root.chatPage.SetLayoutSettings(root.layoutSettings)
			root.resizeAll()
			root.editor.Blur()
			root.chatPage.BlurMessages()
			root.focusedPanel = PanelContent
			updatedPage, _ := root.chatPage.Update(runtime.UserMessage("pointer hover target", sess.ID, nil))
			root.chatPage = updatedPage.(chat.Page)
			root.chatPages[sess.ID] = root.chatPage
			root.viewCacheValid = false
			base := ansi.Strip(root.View().Content)
			page := root.chatPage.(chat.SplitPresentation)
			layout := page.MeasureSplitShell(root.width, root.mainContentHeight())
			x := layout.TranscriptArea.X + 3
			y := layout.TranscriptArea.Y + root.mainContentInset() + 1
			require.Contains(t, base, "pointer hover target")

			updated, cmd := root.updateWithLifecycle(messages.PointerUpdateMsg{X: x, Y: y, Motion: &tea.MouseMotionMsg{X: x, Y: y}})
			root = updated.(*appModel)
			for step := 0; step < 8 && root.ar.ActiveCount() > 0; step++ {
				var tick tea.Msg
				for _, next := range collectMsgs(cmd) {
					if _, ok := next.(animation.TickMsg); ok {
						tick = next
						break
					}
				}
				if tick == nil {
					break
				}
				_, cmd = root.updateWithLifecycle(tick)
			}
			hovered := ansi.Strip(root.View().Content)
			copyLine := -1
			for line, content := range strings.Split(hovered, "\n") {
				if _, _, ok := strings.Cut(content, types.MessageCopyLabel); ok {
					copyLine = line
					break
				}
			}
			require.GreaterOrEqual(t, copyLine, 0, "settled pointer hover shows Copy")
			require.Equal(t, root.mainContentInset(), copyLine, "single message action row starts below the app inset")

			// The fade may still paint the action row, but the action ceases to be a hit target immediately.
			composerY := root.editorTop() + 1
			motion := tea.MouseMotionMsg{X: root.composerColumn().X + 2, Y: composerY}
			_, cmd = root.updateWithLifecycle(messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion})
			for step := 0; step < 3 && root.ar.ActiveCount() > 0; step++ {
				var tick tea.Msg
				for _, next := range collectMsgs(cmd) {
					if _, ok := next.(animation.TickMsg); ok {
						tick = next
						break
					}
				}
				if tick == nil {
					break
				}
				_, cmd = root.updateWithLifecycle(tick)
			}
			for step := 0; step < 5 && strings.Contains(ansi.Strip(root.View().Content), types.MessageCopyLabel); step++ {
				if root.ar.ActiveCount() == 0 {
					break
				}
				cmd = root.ar.Continue()
				require.NotNil(t, cmd, "fade-out has a scheduled tick")
				updated, _ = root.updateWithLifecycle(cmd())
				root = updated.(*appModel)
			}
			require.NotContains(t, ansi.Strip(root.View().Content), types.MessageCopyLabel, "bounded fade-out removes stale Copy pixels")
		})
	}
}
