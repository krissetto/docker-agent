package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

func TestInputIdentityFullTUIPointerAttachesChildAndSelectsParent(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(filepath.Join(dir, "data"))
	paths.SetConfigDir(filepath.Join(dir, "config"))
	t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })
	parent := session.New(session.WithID("parent-full-session-id"))
	child := session.New(session.WithID("child-full-session-id"))
	child.ParentID = parent.ID
	tree := subagent.Snapshot{Root: subagent.SessionRootID(parent.ID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(parent.ID), Agent: "director"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "a1b2c", SessionID: child.ID, Parent: subagent.SessionRootID(parent.ID), Agent: "worker"}}}}}}
	parent.SetSubagentTree(&tree)
	child.SetSubagentTree(&tree)
	notice := session.UserMessage("private runtime payload")
	notice.InputOrigin, notice.InputMode, notice.SenderID, notice.SenderName = session.InputOriginRuntime, "steer", child.ID, "worker"
	parent.AddMessage(notice)
	delegation := session.UserMessage("CHILD-DELEGATION-BODY")
	delegation.InputOrigin, delegation.InputMode, delegation.SenderID, delegation.SenderName = session.InputOriginAgent, "turn", parent.ID, "director"
	child.AddMessage(delegation)
	info := runtime.SubagentAttachInfo{NodeID: "a1b2c", Session: child, Agent: "worker", ParentSessionID: parent.ID, ParentAgent: "director"}
	services := &openSubagentRuntime{closeTabRuntime: newCloseTabRuntime("root", false), info: info}
	application := app.New(t.Context(), &openSubagentSessions{}, parent, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	spawner := func(ctx context.Context, workingDir string) (SpawnedSession, error) { return SpawnedSession{}, nil }
	root := New(t.Context(), spawner, application, dir, func() {}).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	driver := tuitest.New(t, root, 120, 40)
	driver.WaitFor(tuitest.Contains("worker (a1b2c) has replied >"))
	click := func(label string) {
		t.Helper()
		frame := driver.Frame()
		for y, line := range strings.Split(frame, "\n") {
			before, _, found := strings.Cut(ansi.Strip(line), label)
			if !found {
				continue
			}
			x := ansi.StringWidth(before)
			driver.Send(tea.MouseMotionMsg{X: x, Y: y})
			hovered := driver.Frame()
			hoveredLine := strings.Split(ansi.Strip(hovered), "\n")[y]
			assert.Equal(t, ansi.Strip(line), hoveredLine, "identity row stays at the same screen coordinates")
			t.Logf("actual full TUI click label=%q x=%d y=%d\n%s", label, x, y, ansi.Strip(hovered))
			driver.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			return
		}
		t.Fatalf("identity %q missing from frame:\n%s", label, ansi.Strip(frame))
	}
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click(">")
	driver.WaitFor(tuitest.Contains("private runtime payload"))
	click("v")
	driver.WaitFor(tuitest.Contains("worker (a1b2c) has replied >"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("worker (a1b2c)")
	driver.WaitFor(tuitest.Contains("CHILD-DELEGATION-BODY"))
	driver.WaitFor(tuitest.Contains("↳"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("director (paren)")
	driver.WaitFor(tuitest.Contains("worker (a1b2c) has replied >"))
}
