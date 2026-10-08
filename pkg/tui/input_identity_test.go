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
	parent := session.New(session.WithID("parent-full-session-id"), session.WithAgentName("director"))
	child := session.New(session.WithID("child-full-session-id"), session.WithAgentName("worker"))
	parent.SetAttribute(runtime.SessionAgentAttribute, "director")
	child.SetAttribute(runtime.SessionAgentAttribute, "worker")
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
	sessions := &preparedViewSessions{root: parent, views: map[string]runtime.SubagentAttachInfo{child.ID: info}}
	application := app.New(t.Context(), sessions, parent, runtime.SessionBinding{AgentName: "director"}, app.WithRuntimeServices(services))
	spawner := func(ctx context.Context, workingDir string) (SpawnedSession, error) { return SpawnedSession{}, nil }
	root := New(t.Context(), spawner, application, dir, func() {}).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	driver := tuitest.New(t, root, 120, 40)
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received >"))
	click := func(label string, double bool) {
		t.Helper()
		frame := driver.Frame()
		for y, line := range strings.Split(frame, "\n") {
			before, _, found := strings.Cut(ansi.Strip(line), label)
			if label == "v" {
				if i := strings.LastIndex(ansi.Strip(line), " v"); i >= 0 {
					before, found = ansi.Strip(line)[:i+1], true
				}
			}
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
			if double {
				if label == "worker (a1b2c)" {
					assert.NotContains(t, ansi.Strip(driver.Frame()), "director (paren) sent a message", "first identity click discloses without attaching")
				} else {
					assert.Contains(t, ansi.Strip(driver.Frame()), "director (paren) sent a message", "first parent-identity click discloses without switching")
				}
				driver.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			return
		}
		t.Fatalf("identity %q missing from frame:\n%s", label, ansi.Strip(frame))
	}
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click(">", false)
	driver.WaitFor(tuitest.Contains("private runtime payload"))
	click("v", false)
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received >"))
	// The chevron follows the target state before the closing animation ends.
	driver.WaitFor(tuitest.Absent("private runtime payload"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("worker (a1b2c)", true)
	driver.WaitFor(tuitest.Contains("director (paren) sent a message >"))
	driver.WaitFor(tuitest.Contains("↳"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "CHILD-DELEGATION-BODY", "attached message card starts collapsed")
	click(">", false)
	driver.WaitFor(tuitest.Contains("CHILD-DELEGATION-BODY"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("director (paren)", true)
	// The first child-identity click disclosed this retained parent card.
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received v"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "CHILD-DELEGATION-BODY")
}
