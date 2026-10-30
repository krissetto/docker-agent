package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type scrollbarPointerPage struct {
	chat.Page

	x, y       int
	generation uint64
	updates    int
	active     bool
}

func (p *scrollbarPointerPage) SetScrollbarsActive(active bool) { p.active = active }

func (p *scrollbarPointerPage) SetScrollbarPointer(x, y int) {
	if p.x != x || p.y != y {
		p.generation++
	}
	p.x, p.y = x, y
}

func (p *scrollbarPointerPage) VisualGeneration() uint64 {
	return p.Page.VisualGeneration() + p.generation
}

func (p *scrollbarPointerPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	p.updates++
	_, cmd := p.Page.Update(msg)
	return p, cmd
}

func pointerPage(root *appModel, id string) *scrollbarPointerPage {
	p := &scrollbarPointerPage{Page: root.chatPages[id], x: -1, y: -1}
	root.chatPages[id] = p
	if id == root.paneFocus() {
		root.chatPage = p
	}
	return p
}

func TestScrollbarPointerObservesAllVisiblePanesWithoutDispatch(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	first, second := pointerPage(root, "profile"), pointerPage(root, "second")
	hidden := pointerPage(root, "third")
	for _, point := range [][2]int{{10, 4}, {90, 4}, {20, root.height - 1}, {-10, -10}} {
		root.viewCacheValid = true
		root.syncScrollbarPointer(tea.MouseMotionMsg{X: point[0], Y: point[1]})
		for _, p := range []*scrollbarPointerPage{first, second} {
			want := point
			got := [2]int{p.x, p.y}
			require.Equal(t, want, got)
			require.Zero(t, p.updates, "hover observation must not repeat selection/drag dispatch")
		}
		require.Equal(t, [2]int{-1, -1}, [2]int{hidden.x, hidden.y})
		require.False(t, root.viewCacheValid)
	}
}

func TestScrollbarPointerClearsOnOcclusionWithoutMouseMotion(t *testing.T) {
	for _, reason := range []string{"modal", "blur", "resize-drag", "lean"} {
		t.Run(reason, func(t *testing.T) {
			root := splitTestRoot(t)
			p := pointerPage(root, "profile")
			root.syncScrollbarPointer(tea.MouseMotionMsg{X: 12, Y: 4})
			require.Equal(t, [2]int{12, 4}, [2]int{p.x, p.y})
			var event tea.Msg = tea.KeyPressMsg{Code: 'a'}
			switch reason {
			case "modal":
				root.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "hover-occlusion"}})
			case "blur":
				root.blurred = true
				event = tea.BlurMsg{}
			case "resize-drag":
				root.isDragging = true
			case "lean":
				root.leanMode = true
			}
			root.syncScrollbarPointer(event)
			if reason == "lean" {
				require.Equal(t, [2]int{12, 4}, [2]int{p.x, p.y}, "lean routing is unchanged")
			} else {
				require.Equal(t, [2]int{-1, -1}, [2]int{p.x, p.y})
				if reason != "resize-drag" {
					require.False(t, p.active)
				}
			}
		})
	}
}

func TestScrollbarPointerSurvivesNonPointerUpdates(t *testing.T) {
	root := splitTestRoot(t)
	p := pointerPage(root, "profile")
	root.syncScrollbarPointer(messages.WheelCoalescedMsg{X: 17, Y: 8, Delta: 1})
	root.syncScrollbarPointer(tea.KeyPressMsg{Code: 'x'})
	require.Equal(t, [2]int{17, 8}, [2]int{p.x, p.y})
	root.syncScrollbarPointer(tea.BlurMsg{})
	root.syncScrollbarPointer(tea.FocusMsg{})
	require.Equal(t, [2]int{-1, -1}, [2]int{p.x, p.y}, "focus does not revive stale pointer proximity")
}

func TestScrollbarPointerLifecycleClearsWhenDialogOpens(t *testing.T) {
	root := splitTestRoot(t)
	p := pointerPage(root, "profile")
	root.Update(tea.MouseMotionMsg{X: 12, Y: 4})
	require.Equal(t, [2]int{12, 4}, [2]int{p.x, p.y})
	root.Update(dialog.OpenDialogMsg{Model: &stubDialog{id: "hover-occlusion"}})
	require.Equal(t, [2]int{-1, -1}, [2]int{p.x, p.y})
}
