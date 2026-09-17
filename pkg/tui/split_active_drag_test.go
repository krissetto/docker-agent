package tui

import (
	"math"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActivePaneSelfEdgeUsesNextHiddenTab(t *testing.T) {
	for _, edge := range []splitEdge{splitLeft, splitRight, splitTop, splitBottom} {
		root := splitTestRoot(t)
		original := root.paneLayout()
		root.editor.SetValue("keep active draft")
		next, load, ok := root.paneSplitCandidate(original, "profile", "profile", edge)
		require.True(t, ok)
		require.Equal(t, "second", load)
		require.Len(t, next.Sessions(), 2)
		require.Equal(t, []string{"profile"}, original.Sessions())
		root.splitPane("profile", "profile", edge)
		require.Equal(t, "profile", root.paneFocus())
		require.Equal(t, "keep active draft", root.editor.Value())
		require.Equal(t, 3, root.supervisor.Count())
	}
}

func TestActiveNestedSelfEdgePreservesUnrelatedRatioAndUniqueLeaves(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	before := root.panes.root
	next, load, ok := root.paneSplitCandidate(root.panes, "second", "second", splitBottom)
	require.True(t, ok)
	require.Equal(t, "third", load)
	require.Equal(t, math.Float64bits(before.ratio), math.Float64bits(next.root.ratio), "unrelated ratio remains bit-for-bit identical")
	require.Equal(t, "profile", next.root.first.session)
	require.Equal(t, []string{"profile", "second"}, root.panes.Sessions())
	root.splitPane("second", "second", splitBottom)
	require.Equal(t, []string{"profile", "third", "second"}, root.panes.Sessions())
	unchanged := root.panes.root
	_, _, ok = root.paneSplitCandidate(root.panes, "second", "second", splitLeft)
	require.False(t, ok, "all visible: self-edge cannot duplicate or displace")
	require.Same(t, unchanged, root.panes.root)
}

func TestActiveColdRemainderLoadsOnlyCommittedDrop(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		root, store := coldPaneRoot(t)
		// Make every warm alternative visible; only cold can fill the remainder.
		root.splitPane("second", "profile", splitRight)
		root.splitPane("third", "second", splitBottom)
		root.handleWindowResize(220, 70)
		root.handleSwitchTab("profile")
		root.editor.SetValue("active draft")
		x, y := paneTabPoint(t, root, "profile")
		r := root.paneGeometry.Panes["profile"]
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		root.Update(tea.MouseMotionMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft})
		require.Zero(t, store.reads.Load())
		require.Nil(t, root.chatPages["cold"])
		before := root.panes.root
		_, cmd := root.Update(tea.MouseReleaseMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft})
		require.Same(t, before, root.panes.root)
		require.NotNil(t, root.paneHydration)
		if cancel {
			root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		}
		for _, msg := range collectMsgs(cmd) {
			if loaded, ok := msg.(paneHydratedMsg); ok {
				root.finishPaneHydration(loaded)
			}
		}
		require.EqualValues(t, 1, store.reads.Load())
		require.Equal(t, "profile", root.paneFocus())
		require.Equal(t, "active draft", root.editor.Value())
		if cancel {
			require.Same(t, before, root.panes.root)
			require.Nil(t, root.chatPages["cold"])
		} else {
			require.Len(t, root.panes.Sessions(), 4)
			require.NotNil(t, root.chatPages["cold"])
			require.Empty(t, root.pendingRestores["cold"])
		}
	}
}

func TestActiveColdRemainderPreviewCancellationDoesNotLoad(t *testing.T) {
	root, store := coldPaneRoot(t)
	root.supervisor.CloseSession("second")
	root.supervisor.CloseSession("third")
	tabs, active := root.supervisor.GetTabs()
	root.setTabs(tabs, active)
	x, y := paneTabPoint(t, root, "profile")
	_, bounds, _ := root.measurePanes()
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	motion := tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft}
	root.Update(messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion})
	root.Update(tea.BlurMsg{})
	require.Zero(t, store.reads.Load())
	require.Nil(t, root.chatPages["cold"])
	require.Equal(t, []string{"profile"}, root.panes.Sessions())
}
