package tui

import (
	"bytes"
	"encoding/base64"
	stdimage "image"
	"image/png"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestChatImagePreviewUsesClickedLeaseAndRejectsGoneOwner(t *testing.T) {
	root := splitTestRoot(t)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 40, 30))))
	source, ok := tuiimage.FromBytes("clicked-second.png", "image/png", data.Bytes())
	require.True(t, ok)
	var output bytes.Buffer
	root.imageWriter = tuiimage.NewWriter(&output)
	root.imageWriter.SetEnabled(false)
	lease := tuiimage.PreviewFromInline(source)
	root.Update(messages.OpenImagePreviewMsg{SessionID: "second", Preview: lease})
	require.True(t, root.dialogMgr.Open())
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "clicked-second.png")
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "cagent-image;")
	assert.Equal(t, "profile", root.paneFocus(), "source identity never comes from whichever pane is focused on async delivery")
	root.dialogMgr.Cleanup()
	_, err := lease.Fit(10, 10, tuiimage.CellSize{})
	require.Error(t, err, "dialog cleanup releases transferred lease")
	gone := tuiimage.PreviewFromInline(source)
	root.Update(messages.OpenImagePreviewMsg{SessionID: "gone", Preview: gone})
	_, err = gone.Fit(10, 10, tuiimage.CellSize{})
	require.Error(t, err)
}
func TestChatImageUnsupportedFallbackReleasesLease(t *testing.T) {
	root := splitTestRoot(t)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2))))
	source, ok := tuiimage.FromBytes("image.png", "image/png", data.Bytes())
	require.True(t, ok)
	lease := tuiimage.PreviewFromInline(source)
	root.Update(messages.OpenImagePreviewMsg{SessionID: "profile", Preview: lease})
	require.True(t, root.dialogMgr.Open())
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "does not support")
	_, err := lease.Fit(10, 10, tuiimage.CellSize{})
	require.Error(t, err)
	root.dialogMgr.Cleanup()
}

func TestRoutedChatImageRetainsOriginAndReleasesStaleGeneration(t *testing.T) {
	root := splitTestRoot(t)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 2))))
	source, ok := tuiimage.FromBytes("background.png", "image/png", data.Bytes())
	require.True(t, ok)
	generation, ok := root.supervisor.RouteGeneration("second")
	require.True(t, ok)
	lease := tuiimage.PreviewFromInline(source)
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation, Inner: messages.OpenImagePreviewMsg{SessionID: "second", Preview: lease}})
	require.True(t, root.dialogMgr.Open(), "clicked source may open after focus moved; it must not select current session's image")
	root.dialogMgr.Cleanup()
	stale := tuiimage.PreviewFromInline(source)
	root.Update(messages.RoutedMsg{SessionID: "second", RouteGeneration: generation + 1, Inner: messages.OpenImagePreviewMsg{SessionID: "second", Preview: stale}})
	_, err := stale.Fit(10, 10, tuiimage.CellSize{})
	require.Error(t, err, "stale routed commands must release payload lease")
}

func TestSplitChatImageClickUsesPaintedInactivePane(t *testing.T) {
	root := splitTestRoot(t)
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 20, 20))))
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
	runner := root.supervisor.GetRunner("second")
	runner.App.Session().AddMessage(&session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: "![SECOND-IMAGE](" + uri + ")"}})
	root.createSessionComponents("second", runner.App, runner.App.Session())
	var deliver func(tea.Cmd)
	deliver = func(cmd tea.Cmd) {
		for _, msg := range collectMsgs(cmd) {
			if _, tick := msg.(animation.TickMsg); tick {
				continue
			}
			_, next := root.Update(msg)
			if next != nil {
				deliver(next)
			}
		}
	}
	deliver(root.routePaneCmd("second", root.chatPages["second"].Init()))
	root.splitPane("second", "profile", splitBottom)
	root.handleSwitchTab("profile")
	frame := root.View().Content
	x, y := -1, -1
	bounds := root.paneGeometry.Panes["second"]
	for row, line := range strings.Split(frame, "\n") {
		if row < bounds.Y || row >= bounds.Y+bounds.H-root.paneHeaderHeight() {
			continue
		}
		if at := strings.Index(line, "\x1b_cagent-image;"); at >= 0 {
			x = ansi.StringWidth(line[:at])
			y = row
			break
		}
	}
	require.GreaterOrEqual(t, x, 0, "real inactive lower pane must contain loaded image")
	_, press := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	deliver(press)
	require.False(t, root.dialogMgr.Open(), "press alone never previews")
	_, release := root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	deliver(release)
	require.True(t, root.dialogMgr.Open(), "release should preview clicked inactive pane image after focus transition")
	assert.Contains(t, root.dialogMgr.TopDialog().View(), "SECOND-IMAGE")
	root.dialogMgr.Cleanup()
}
