package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

func TestAttachmentBannerMeaningfulOverflowRouting(t *testing.T) {
	root, _, _ := wallClockRoot(t, 50, 40)
	defer root.ar.Stop()
	for i := range 4 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("attachment-%d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("exact content"), 0o600))
		require.NoError(t, root.editor.AttachFile(path))
	}
	root.resizeAll()
	banner := root.editor.(editor.BannerLayout)
	draft := root.editor.Value()
	before := root.editor.BannerHeight()
	for _, point := range [][2]int{{0, 0}, {20, 0}, {20, 1}, {0, 2}} {
		root.handleMouseClick(tea.MouseClickMsg{X: point[0], Y: root.composerLayout().bannerTop + point[1], Button: tea.MouseLeft})
		require.Equal(t, before, root.editor.BannerHeight())
		require.False(t, root.editor.IsContextBarFocused(), "blank and border clicks do not steal focus")
	}
	x := -1
	for column := range root.width {
		if banner.ContextBarToggleAt(column, 2) {
			x = column
			break
		}
	}
	require.NotEqual(t, -1, x)
	root.handleMouseClick(tea.MouseClickMsg{X: x, Y: root.composerLayout().bannerTop + 2, Button: tea.MouseLeft})
	require.Greater(t, root.editor.BannerHeight(), before)
	require.True(t, root.editor.IsContextBarFocused())
	for _, key := range []tea.KeyPressMsg{{Code: ' ', Text: " "}, {Code: tea.KeyEnter}} {
		root.Update(key)
	}
	require.Greater(t, root.editor.BannerHeight(), before)
	root.handleWindowResize(220, 40)
	require.Equal(t, before, root.editor.BannerHeight(), "widening collapses before layout reads banner height")
	for _, key := range []tea.KeyPressMsg{{Code: ' ', Text: " "}, {Code: tea.KeyEnter}} {
		root.Update(key)
		require.Equal(t, before, root.editor.BannerHeight())
		require.Equal(t, draft, root.editor.Value())
	}
	require.NotContains(t, ansi.Strip(root.editor.BannerView(220)), "▸")
}

func TestAttachmentBannerPreparedHeightAnchorsCompletion(t *testing.T) {
	root, _, _ := wallClockRoot(t, 45, 40)
	defer root.ar.Stop()
	for i := range 4 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("attachment-%d.txt", i))
		require.NoError(t, os.WriteFile(path, []byte("content"), 0o600))
		require.NoError(t, root.editor.AttachFile(path))
	}
	root.resizeAll()
	root.editor.ToggleContextBar()
	root.resizeAll()
	require.Greater(t, root.editor.BannerHeight(), 3)
	root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "@candidate", Value: "@candidate"}}})
	for _, size := range [][2]int{{45, 40}, {220, 40}, {45, 12}, {45, 40}} {
		root.handleWindowResize(size[0], size[1])
		geometry := root.composerLayout()
		height := root.editor.BannerHeight()
		layers := root.completions.GetLayers()
		if len(layers) > 0 {
			require.Equal(t, geometry.bannerTop, layers[0].GetY()+layers[0].Height())
		}
		view := root.editor.BannerView(root.width)
		require.Equal(t, height, root.editor.BannerHeight(), "paint must not discover a different height")
		if view != "" {
			require.Equal(t, height, lipgloss.Height(view))
		}
		require.Equal(t, root.height, len(strings.Split(root.composeView().Content, "\n")))
		if size[0] == 220 {
			require.Equal(t, 3, height)
		}
	}
}
