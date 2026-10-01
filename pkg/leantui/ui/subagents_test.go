package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func pickerTree() subagent.Snapshot {
	return subagent.Snapshot{Root: "stable-root", Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "stable-root", Name: "Director", SessionID: "root-session", State: subagent.NodeRunning},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "first", Name: "Worker", State: subagent.NodeRunning}, Children: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "nested", Name: "Worker", State: subagent.NodeIdle}},
			}},
			{Node: subagent.Node{ID: "second", Name: "Worker", State: subagent.NodeCompleted}},
		},
	}}}
}

func TestSubagentPickerHierarchyAndRestoredRoot(t *testing.T) {
	p := NewSubagentPicker(pickerTree(), "root-session", "")
	assert.Equal(t, subagent.NodeID("stable-root"), p.selected)
	p.Navigate(KeyRight)
	assert.Equal(t, subagent.NodeID("first"), p.selected)
	p.Navigate(KeyRight)
	assert.Equal(t, subagent.NodeID("nested"), p.selected)
	p.Navigate(KeyLeft)
	assert.Equal(t, subagent.NodeID("first"), p.selected)
	p.Navigate(KeyLeft)
	assert.Len(t, p.rows, 3)
	p.Navigate(KeyDown)
	assert.Equal(t, subagent.NodeID("second"), p.selected)
	p.Navigate(KeyUp)
	p.Navigate(KeyRight)
	assert.Len(t, p.rows, 4)
	assert.Equal(t, subagent.NodeID("first"), p.selected)
	p.Navigate(KeyEnd)
	assert.Equal(t, subagent.NodeID("second"), p.selected)
	p.Navigate(KeyHome)
	assert.Equal(t, subagent.NodeID("stable-root"), p.selected)
	p.Navigate(KeyUp)
	assert.Equal(t, subagent.NodeID("second"), p.selected)
	p.Navigate(KeyDown)
	assert.Equal(t, subagent.NodeID("stable-root"), p.selected)

	attached := NewSubagentPicker(pickerTree(), "child-session", "nested")
	assert.Equal(t, subagent.NodeID("nested"), attached.selected)
	assert.Len(t, attached.rows, 4, "keep ancestors and siblings as context")
}

func TestSubagentPickerLiveIdentityAndRemoval(t *testing.T) {
	snapshot := pickerTree()
	p := NewSubagentPicker(snapshot, "child-session", "nested")
	snapshot.Nodes[0].Children[1].Node.CreatedAt = time.Now()
	snapshot.Nodes[0].Children[0].Children[0].Node.State = subagent.NodeCompleted
	p.Update(snapshot)
	node, ok := p.Current()
	require.True(t, ok)
	assert.Equal(t, subagent.NodeID("nested"), node.ID)
	assert.Equal(t, subagent.NodeCompleted, node.State)
	snapshot.Nodes[0].Children[0].Children = nil
	p.Update(snapshot)
	_, ok = p.Current()
	assert.False(t, ok, "removed selection must not attach the next row")
	assert.Contains(t, strings.Join(p.Render(80, 10), "\n"), "Selection no longer available")
	p.Navigate(KeyDown)
	_, ok = p.Current()
	assert.True(t, ok, "explicit navigation can choose a new target")
	p.Update(subagent.Snapshot{})
	_, ok = p.Current()
	assert.False(t, ok)
}

func TestSubagentPickerFrameBoundsAndArtifact(t *testing.T) {
	s := NewScreen("", "", "")
	s.Editor.SetText("unsent draft")
	s.Subagents = NewSubagentPicker(pickerTree(), "child-session", "nested")
	for _, width := range []int{1, 8, 24, 60, 100} {
		for _, height := range []int{1, 2, 3, 4, 8, 20} {
			lines, row, col := s.Frame(width, height, 0, true, nil, nil)
			assert.LessOrEqual(t, len(lines), height)
			assert.Less(t, row, height)
			assert.Less(t, col, width)
			for _, line := range lines {
				assert.LessOrEqual(t, DisplayWidth(line), width)
			}
		}
	}
	lines, _, _ := s.Frame(60, 8, 0, true, nil, nil)
	text := ansi.Strip(strings.Join(lines, "\n"))
	assert.Contains(t, text, "Director")
	assert.Contains(t, text, "│ └─")
	assert.NotContains(t, text, "stable-root")
	assert.NotContains(t, text, "child-session")
	assert.NotContains(t, text, "unsent draft")
	assert.Equal(t, "unsent draft", s.Editor.Text())

	// Optional scratch artifact: does not change the repository during tests.
	if path := os.Getenv("LEAN_SUBAGENT_RENDER_ARTIFACT"); path != "" {
		var artifact strings.Builder
		for _, size := range [][2]int{{60, 8}, {24, 5}, {8, 3}} {
			lines, _, _ := s.Frame(size[0], size[1], 0, true, nil, nil)
			fmt.Fprintf(&artifact, "%dx%d\n%s\n\n", size[0], size[1], ansi.Strip(strings.Join(lines, "\n")))
		}
		s.Subagents.Update(subagent.Snapshot{})
		fmt.Fprintf(&artifact, "empty\n%s\n", ansi.Strip(strings.Join(s.Subagents.Render(60, 8), "\n")))
		require.NoError(t, os.WriteFile(path, []byte(artifact.String()), 0o600))
	}
}

func TestSubagentPickerKeepsEnclosingRootAcrossLiveUpdates(t *testing.T) {
	snapshot := pickerTree()
	snapshot.Nodes = append([]subagent.NodeSnapshot{{Node: subagent.Node{ID: "unrelated", Name: "Other root", SessionID: "other-session"}}}, snapshot.Nodes...)
	snapshot.Root = "unrelated"
	p := NewSubagentPicker(snapshot, "child-session", "nested")
	assert.Equal(t, subagent.NodeID("stable-root"), p.root)
	assert.Equal(t, subagent.NodeID("nested"), p.selected)
	assert.Len(t, p.rows, 4)
	assert.NotContains(t, strings.Join(p.Render(80, 10), "\n"), "Other root")
	snapshot.Nodes[1].Children[0].Children = nil
	p.Update(snapshot)
	assert.Empty(t, p.selected)
	assert.Len(t, p.rows, 3)
	snapshot.Nodes = snapshot.Nodes[:1]
	p.Update(snapshot)
	assert.Empty(t, p.rows, "do not jump to an unrelated runtime root")
}

func TestSubagentPickerInitiallyEmptyReceivesLiveTree(t *testing.T) {
	p := NewSubagentPicker(subagent.Snapshot{}, "root-session", "")
	assert.Empty(t, p.rows)
	p.Navigate(KeyUp)
	p.Navigate(KeyDown)
	p.Update(pickerTree())
	assert.Len(t, p.rows, 4)
	assert.Equal(t, subagent.NodeID("stable-root"), p.selected)
}
