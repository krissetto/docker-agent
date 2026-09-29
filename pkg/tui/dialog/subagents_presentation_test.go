package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func subagentsPresentationFixture() *subagentsDialog {
	nodes := []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "parent-full", Agent: "director", SessionID: "parent-session", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "child-full", Agent: "工作 👩‍💻", SessionID: "child-session", State: subagent.NodeRunning}},
			{Node: subagent.Node{ID: "child-two", Agent: "worker", State: subagent.NodeIdle}},
		}},
		{Node: subagent.Node{ID: "other-full", Agent: "director", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "other-child", Agent: "worker"}}}},
	}
	d := NewSubagentsDialog(nodes, map[string]string{"parent-session": "Generated title 世界", "child-session": strings.Repeat("Unicode 👩‍💻 é 世界 title ", 8)}).(*subagentsDialog)
	d.SetSize(100, 32)
	return d
}

func settleSubagentsHover(t *testing.T, d *subagentsDialog, ar *animation.Runtime) {
	t.Helper()
	for range 60 {
		if !d.hoverAnimation.IsActive() {
			require.Zero(t, ar.ActiveCount())
			require.Nil(t, ar.Continue())
			return
		}
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		d.Update(acceptedDialogTick(ar, cmd))
	}
	t.Fatal("hover did not quiesce")
}

func TestSubagentsHoverIsFiniteAndIndependentOfSelection(t *testing.T) {
	d := subagentsPresentationFixture()
	ar := newDialogRuntime()
	d.BindAnimationRuntime(ar)
	t.Cleanup(d.Cleanup)
	x, y := d.bodyX+5, d.bodyY+d.prepared[1].start
	before := d.View()
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, subagent.NodeID("parent-full"), d.selectedID())
	require.Equal(t, subagent.NodeID("child-full"), d.hovered)
	require.Zero(t, d.hover["child-full"].value)
	d.Update(acceptedDialogTick(ar, ar.Continue()))
	require.Greater(t, d.hover["child-full"].value, 0.0)
	require.Less(t, d.hover["child-full"].value, 1.0)
	require.NotEqual(t, before, d.View())
	require.Equal(t, ansi.Strip(before), ansi.Strip(d.View()))
	settleSubagentsHover(t, d, ar)
	require.Equal(t, 1.0, d.hover["child-full"].value)
	d.TakeVisualDirty()
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.False(t, d.TakeVisualDirty())
	require.Zero(t, ar.ActiveCount())
	d.Update(tea.MouseMotionMsg{X: 0, Y: 0})
	d.Update(acceptedDialogTick(ar, ar.Continue()))
	require.Greater(t, d.hover["child-full"].value, 0.0)
	require.Less(t, d.hover["child-full"].value, 1.0)
	settleSubagentsHover(t, d, ar)
	require.Empty(t, d.hover)
	require.Equal(t, before, d.View())
}

func TestSubagentsGeometryTitlesSeparatorsAndSidebarRoots(t *testing.T) {
	d := subagentsPresentationFixture()
	lines, geometry, owners := d.renderRows(d.prepared[0].width)
	require.Len(t, geometry, 5)
	require.NotContains(t, ansi.Strip(lines[geometry[0].start]), "├")
	require.NotContains(t, ansi.Strip(lines[geometry[3].start]), "└")
	require.Contains(t, ansi.Strip(lines[geometry[1].start]), "├ ")
	require.Contains(t, ansi.Strip(lines[geometry[2].start]), "└ ")
	require.NotContains(t, ansi.Strip(lines[geometry[1].start]), "│ ├")
	require.Equal(t, 3, geometry[1].end-geometry[1].start, "long title gets at most two lines")
	for line, index := range owners {
		x, y := d.bodyX+5, d.bodyY+line
		require.Equal(t, index, d.rowAt(x, y))
		if index < 0 {
			before := d.selectedID()
			_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Nil(t, cmd)
			require.Equal(t, before, d.selectedID())
			d.Update(tea.MouseMotionMsg{X: x, Y: y})
			require.Empty(t, d.hovered)
		}
	}
	// Titles are part of the same agent hit target, including wrapped continuations.
	y := d.bodyY + geometry[1].end - 1
	d.Update(tea.MouseClickMsg{X: d.bodyX + 5, Y: y, Button: tea.MouseLeft})
	require.Equal(t, subagent.NodeID("child-full"), d.selectedID())
	_, cmd := d.Update(tea.MouseClickMsg{X: d.bodyX + 5, Y: y, Button: tea.MouseLeft})
	var opened string
	for _, msg := range collectMsgs(cmd) {
		if msg, ok := msg.(messages.OpenSubagentMsg); ok {
			opened = msg.NodeID
		}
	}
	require.Equal(t, "child-full", opened)
}

func TestSubagentsScrollResizeAndFoldRehitStationaryPointer(t *testing.T) {
	d := subagentsPresentationFixture()
	for i := range 80 {
		d.nodes = append(d.nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprintf("agent-%d", i)), Agent: "worker", State: subagent.NodeIdle}})
	}
	d.rebuild("")
	d.SetSize(100, 20)
	x, y := d.bodyX+5, d.bodyY+1
	d.Update(tea.MouseMotionMsg{X: x, Y: y})
	first := d.hovered
	d.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: 5})
	require.Positive(t, d.scrollview.ScrollOffset())
	index := d.rowAt(x, y)
	require.GreaterOrEqual(t, index, 0)
	require.Equal(t, d.rows[index].Node.ID, d.hovered)
	require.NotEqual(t, first, d.hovered)
	d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, d.hovered, d.selectedID())
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Equal(t, subagent.NodeID("agent-79"), d.selectedID())
	require.Contains(t, ansi.Strip(d.View()), "#agent")
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	g := d.prepared[0]
	d.Update(tea.MouseClickMsg{X: d.bodyX + g.chevron, Y: d.bodyY + g.start, Button: tea.MouseLeft})
	require.True(t, d.collapsed["parent-full"])
	for _, row := range d.rows {
		require.NotEqual(t, subagent.NodeID("child-full"), row.Node.ID)
		require.NotEqual(t, subagent.NodeID("child-two"), row.Node.ID)
	}
	for _, size := range [][2]int{{70, 24}, {30, 12}, {15, 8}, {6, 4}, {1, 1}} {
		d.SetSize(size[0], size[1])
		view := d.View()
		require.LessOrEqual(t, lipgloss.Width(view), size[0])
		require.LessOrEqual(t, lipgloss.Height(view), size[1])
		require.NotContains(t, view, "\ufffd")
		index := d.rowAt(d.pointerX, d.pointerY)
		if index < 0 {
			require.Empty(t, d.hovered)
		} else {
			require.Equal(t, d.rows[index].Node.ID, d.hovered)
		}
	}
}

func TestSubagentsThemeKeepsHoverGeometryAndIdentity(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	d := subagentsPresentationFixture()
	d.Update(tea.MouseMotionMsg{X: d.bodyX + 5, Y: d.bodyY + 1})
	before := d.View()
	geometry := append([]subagentRowGeometry(nil), d.prepared...)
	theme := *original
	theme.Colors.TextPrimary = "#19abce"
	theme.Colors.TextSecondary = "#ac48da"
	theme.Colors.CardBg = "#102030"
	styles.ApplyTheme(&theme)
	d.Update(messages.ThemeChangedMsg{})
	require.NotEqual(t, before, d.View())
	require.Equal(t, ansi.Strip(before), ansi.Strip(d.View()))
	require.Equal(t, geometry, d.prepared)
	require.Equal(t, subagent.NodeID("parent-full"), d.hovered)
}

func TestSubagentsOcclusionAndCleanupReleaseHoverLease(t *testing.T) {
	ar := newDialogRuntime()
	mgr := New(ar).(*manager)
	mgr.SetSize(100, 32)
	d := subagentsPresentationFixture()
	mgr.handleOpen(OpenDialogMsg{Model: d})
	mgr.handleTick(advanceDialog(ar, ar.Continue(), dialogOpenDuration))
	mgr.Update(tea.MouseMotionMsg{X: d.bodyX + 5, Y: d.bodyY})
	require.True(t, d.hoverAnimation.IsActive())
	mgr.Update(tea.MouseMotionMsg{X: d.bodyX + 6, Y: d.bodyY})
	require.True(t, d.hoverAnimation.IsActive(), "ordinary motion must not stop hover")
	mgr.handleOpen(OpenDialogMsg{Model: &lifecycleDialog{view: "nested"}})
	require.False(t, d.hoverAnimation.IsActive())
	require.Empty(t, d.hover)
	require.False(t, d.pointerKnown)
	mgr.handleTick(advanceDialog(ar, ar.Continue(), ar.Now()+dialogOpenDuration))
	require.Zero(t, ar.ActiveCount())
	mgr.handleClose()
	mgr.handleTick(advanceDialog(ar, ar.Continue(), ar.Now()+dialogCloseDuration))
	require.Same(t, d, mgr.TopDialog())
	require.Zero(t, ar.ActiveCount())
	mgr.Update(tea.MouseMotionMsg{X: d.bodyX + 5, Y: d.bodyY})
	require.True(t, d.hoverAnimation.IsActive())
	mgr.Cleanup()
	require.Zero(t, ar.ActiveCount())
	require.Nil(t, ar.Continue())
}

func TestSubagentsWaitingAttentionAndScrollbarGeometry(t *testing.T) {
	d := subagentsPresentationFixture()
	d.nodes[0].Node.WaitingOn = "approval 世界"
	d.nodes[0].Node.NeedsAttention = true
	d.rebuild("")
	d.SetSize(45, 16)
	lines, geometry, owners := d.renderRows(d.prepared[0].width)
	body := ansi.Strip(strings.Join(lines[:geometry[0].end], "\n"))
	require.Contains(t, body, "waiting:")
	require.Contains(t, body, "attention")
	require.Len(t, owners, len(lines))
	for _, line := range lines {
		require.LessOrEqual(t, ansi.StringWidth(line), d.prepared[0].width)
	}
	x := d.scrollview.ScrollbarX()
	y := d.bodyY + d.bodyHeight - 1
	selected := d.selectedID()
	d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Positive(t, d.scrollview.ScrollOffset())
	require.Equal(t, selected, d.selectedID())
	require.Empty(t, d.hovered)
	d.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}
