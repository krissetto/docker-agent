package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	driver.WaitFor(tuitest.Contains("Work [+]"))
	click := func(label string) {
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
			// This compares ANSI-stripped text, so it checks coordinates only. The name hover
			// highlight is color-only (agentidentity.HoverProgress via styles.HoverText) and
			// cannot be observed here; pointer routing needs a separate hover-visible check.
			assert.Equal(t, ansi.Strip(line), hoveredLine, "identity row stays at the same screen coordinates")
			t.Logf("actual full TUI click label=%q x=%d y=%d\n%s", label, x, y, ansi.Strip(hovered))
			driver.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			return
		}
		t.Fatalf("identity %q missing from frame:\n%s", label, ansi.Strip(frame))
	}
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	// Compact Work starts collapsed; expand it so the nested report row is reachable.
	click("[+]")
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received >"))
	click(">")
	driver.WaitFor(tuitest.Contains("private runtime payload"))
	click("v")
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received >"))
	// The chevron follows the target state before the closing animation ends.
	driver.WaitFor(tuitest.Absent("private runtime payload"))
	// The collapsed header keeps its surface for a few frames after the body is gone.
	// Snapshot the identity row only once that closing animation has settled.
	driver.WaitFor(tuitest.Absent("┏ · From worker (a1b2c)"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	// The name hover is a color-only highlight (agentidentity.HoverProgress via styles.HoverText),
	// so plain frames cannot show it. Compare the per-cell SGR of the linked name against its
	// neutral ID suffix after the hover animation has settled.
	const nameX, nameY = 12, 3
	restName, restID := identityNameStyles(driver.RawFrame(), nameY)
	driver.Send(tea.MouseMotionMsg{X: nameX, Y: nameY})
	driver.WaitForStable(100 * time.Millisecond)
	hoveredName, hoveredID := identityNameStyles(driver.RawFrame(), nameY)
	assert.Equal(t, restID, hoveredID, "neutral ID suffix keeps its styling under name hover")
	assert.NotEqual(t, restName, hoveredName, "identity name takes the hover color")
	click("worker (a1b2c)")
	driver.WaitFor(tuitest.Contains("director (root:) sent a message v"))
	driver.WaitFor(tuitest.Contains("↳"))
	// An attached session's first incoming assignment opens by default.
	driver.WaitFor(tuitest.Contains("CHILD-DELEGATION-BODY"))
	assert.Equal(t, 1, strings.Count(ansi.Strip(driver.Frame()), "CHILD-DELEGATION-BODY"), "opened attached body renders exactly once")
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("v")
	driver.WaitFor(tuitest.Absent("CHILD-DELEGATION-BODY"))
	// Same closing surface as above: wait for the reply header to settle before the next hover snapshot.
	driver.WaitFor(tuitest.Absent("┏ ✓ From director (root:)"))
	click(">")
	driver.WaitFor(tuitest.Contains("CHILD-DELEGATION-BODY"))
	assert.Equal(t, 1, strings.Count(ansi.Strip(driver.Frame()), "CHILD-DELEGATION-BODY"), "re-expanded attached body renders exactly once")
	assert.NotContains(t, ansi.Strip(driver.Frame()), "private runtime payload")
	click("director (root:)")
	// Attaching the child did not disclose the retained parent card.
	driver.WaitFor(tuitest.Contains("worker (a1b2c) · report received >"))
	assert.NotContains(t, ansi.Strip(driver.Frame()), "CHILD-DELEGATION-BODY")
}

// identityNameStyles returns the SGR state of each cell of the linked agent name
// and of its neutral "(a1b2c)" suffix on row y of a raw frame.
func identityNameStyles(raw string, y int) (name, id []string) {
	lines := strings.Split(raw, "\n")
	if y < 0 || y >= len(lines) {
		return nil, nil
	}
	row := lines[y]
	plain := ansi.Strip(row)
	at := strings.Index(plain, "worker (a1b2c)")
	if at < 0 {
		return nil, nil
	}
	col := utf8.RuneCountInString(plain[:at])
	cells := rowCellSGR(row)
	const nameWidth, idWidth = len("worker"), len("(a1b2c)")
	if col+nameWidth+1+idWidth > len(cells) {
		return nil, nil
	}
	return cells[col : col+nameWidth], cells[col+nameWidth+1 : col+nameWidth+1+idWidth]
}

// rowCellSGR maps each visible cell of a styled row to the SGR sequences active on it.
func rowCellSGR(row string) []string {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var cells []string
	var sgr string
	var state byte
	for row != "" {
		seq, width, n, next := ansi.DecodeSequence(row, state, parser)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			if seq == "\x1b[m" || seq == "\x1b[0m" {
				sgr = ""
			} else {
				sgr += seq
			}
		} else if width > 0 {
			for range utf8.RuneCountInString(seq) {
				cells = append(cells, sgr)
			}
		}
		row = row[n:]
		state = next
	}
	return cells
}
