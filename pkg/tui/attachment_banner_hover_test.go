package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

func bannerRootTick(t *testing.T, root *appModel) {
	t.Helper()
	cmd := root.ar.Continue()
	require.NotNil(t, cmd)
	root.updateWithLifecycle(cmd())
}
func settleRootBanner(t *testing.T, root *appModel) {
	t.Helper()
	for range 160 {
		if root.ar.ActiveCount() == 0 {
			require.Nil(t, root.ar.Continue())
			return
		}
		bannerRootTick(t, root)
	}
	t.Fatal("hover failed to release shared lease")
}
func TestAttachmentBannerHoverRoutesAndClearsBeforeModalPaint(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root, _, _ := harnessRoot(t, 40, 40, ar)
	defer root.ar.Stop()
	defer root.dialogMgr.Cleanup()
	root.editor = editor.New(root.history, editor.WithAnimationRuntime(ar))
	t.Cleanup(root.editor.Cleanup)
	for _, name := range []string{"first.txt", "hidden.txt", "last.txt"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte("PREVIEW-"+name), 0o600))
		require.NoError(t, root.editor.AttachFile(path))
	}
	root.resizeAll()
	rest := root.editor.BannerView(40)
	root.View()
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 2, Y: root.composerLayout().bannerTop + 2})
	require.EqualValues(t, 1, ar.ActiveCount(), "one shared lease for name and count")
	settleRootBanner(t, root)
	hovered := root.editor.BannerView(40)
	require.NotEqual(t, rest, hovered)
	require.Contains(t, root.View().Content, hovered, "dirty hover ticks invalidate root frame")
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 2, Y: root.editorTop() + 1})
	settleRootBanner(t, root)
	require.Equal(t, rest, root.editor.BannerView(40))
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 2, Y: root.composerLayout().bannerTop + 2})
	settleRootBanner(t, root)
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 0, Y: root.composerLayout().bannerTop})
	settleRootBanner(t, root)
	require.NotEqual(t, rest, root.editor.BannerView(40), "blank bar hover highlights count")
	root.handleWindowResize(220, 40)
	settleRootBanner(t, root)
	root.handleWindowResize(40, 40)
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 2, Y: root.composerLayout().bannerTop + 2})
	settleRootBanner(t, root)
	beforeHeight := root.editor.BannerHeight()
	root.updateWithLifecycle(tea.MouseClickMsg{X: 2, Y: root.composerLayout().bannerTop + 2, Button: tea.MouseLeft})
	require.True(t, root.dialogMgr.Open(), "filename takes precedence over whole-bar toggle")
	require.Contains(t, root.dialogMgr.TopDialog().View(), "PREVIEW-first.txt")
	require.Equal(t, beforeHeight, root.editor.BannerHeight())
	require.Equal(t, rest, root.editor.BannerView(40), "opening modal clears hover before first paint")
	root.updateWithLifecycle(tea.MouseMotionMsg{X: 2, Y: root.composerLayout().bannerTop + 2})
	require.Equal(t, rest, root.editor.BannerView(40), "modal cannot reacquire banner hover")

}
