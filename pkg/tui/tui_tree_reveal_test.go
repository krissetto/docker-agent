package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActualProgramSidebarWholeTreeRevealRetargetAndHiddenCleanup(t *testing.T) {
	sess := session.New(session.WithID("reveal-root"), session.WithAgentName("root"))
	sess.Title = "REVEAL-ROOT"
	children := make([]subagent.NodeSnapshot, 8)
	for i := range children {
		children[i] = subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID("reveal-full-node-" + strconv.Itoa(i)), SessionID: "reveal-child-" + strconv.Itoa(i), Agent: "worker", Name: "REVEAL-CHILD-" + strconv.Itoa(i), State: subagent.NodeCompleted}}
	}
	tree := subagent.Snapshot{Root: subagent.SessionRootID(sess.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(sess.ID), SessionID: sess.ID, Agent: "root"}, Children: children}}}
	sess.SetSubagentTree(&tree)
	services := &sidebarAttachRuntime{closeTabRuntime: newCloseTabRuntime("root", false), infos: map[subagent.NodeID]runtime.SubagentAttachInfo{}}
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	root := newSidebarProgramRoot(t, application)
	root.editor.Blur()
	other := session.New(session.WithID("reveal-other"))
	other.Title = "REVEAL-OTHER"
	otherApp := app.New(t.Context(), nil, other, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), otherApp, other, "", nil)
	require.NoError(t, err)
	model := &sidebarHoverProgram{root: root}
	writer := &cacheProgramWriter{}
	program := startTestProgram(t, root, model, tea.WithOutput(writer))
	root.supervisor.SetProgram(program)
	require.Eventually(t, func() bool { return len(model.snapshot()) > 0 }, time.Second, time.Millisecond)
	initial := model.snapshot()
	base := initial[len(initial)-1].content
	_, summaryY := sidebarProgramPoint(t, base, "8 subagents")
	row := ansi.Strip(strings.Split(base, "\n")[summaryY])
	before, _, found := strings.Cut(row, "⌄")
	require.True(t, found)
	x := ansi.StringWidth(before)
	require.Greater(t, x, 110, "whole-tree chevron stays at the sidebar's far-right edge")
	lookups := services.lookups.Load()
	program.Send(tea.MouseClickMsg{X: x, Y: summaryY, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		frames := model.snapshot()
		s := frames[len(frames)-1]
		n := strings.Count(ansi.Strip(s.content), "REVEAL-CHILD-")
		return s.active > 0 && n > 0 && n < 8
	}, time.Second, time.Millisecond, "collapse reveals intermediate row counts")
	intermediate := model.snapshot()
	program.Send(tea.MouseClickMsg{X: x, Y: summaryY, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		frames := model.snapshot()
		s := frames[len(frames)-1]
		return s.active == 0 && strings.Count(ansi.Strip(s.content), "REVEAL-CHILD-") == 8
	}, time.Second, time.Millisecond, "rapid reversal continues from the visible height")
	for _, frame := range intermediate[len(initial):] {
		_, y := sidebarProgramPoint(t, frame.content, "8 subagents")
		require.Equal(t, summaryY, y, "summary row never shifts during animation")
	}
	program.Send(tea.MouseClickMsg{X: x, Y: summaryY, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		frames := model.snapshot()
		s := frames[len(frames)-1]
		return s.active == 0 && !strings.Contains(ansi.Strip(s.content), "REVEAL-CHILD-")
	}, time.Second, time.Millisecond)
	program.Send(tea.MouseClickMsg{X: x - 12, Y: summaryY + 3, Button: tea.MouseLeft})
	require.Equal(t, lookups, services.lookups.Load(), "clipped child rows are not attachment targets")
	// A different tab selection cancels reveal leases, without ticking the hidden page.
	program.Send(tea.MouseClickMsg{X: x, Y: summaryY, Button: tea.MouseLeft})
	program.Send(messages.SwitchTabMsg{SessionID: other.ID})
	require.Eventually(t, func() bool { frames := model.snapshot(); return frames[len(frames)-1].active == 0 }, time.Second, time.Millisecond, "hidden presentation must unregister immediately")
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled awaiting final flush")
	}
	writes := len(writer.snapshot())
	require.Never(t, func() bool { return len(writer.snapshot()) != writes }, 80*time.Millisecond, time.Millisecond)
}
