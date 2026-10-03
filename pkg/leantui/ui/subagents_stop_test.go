package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubagentPickerStopRequiresPinnedConfirmation(t *testing.T) {
	snapshot := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: "root"}}, {Node: subagent.Node{ID: "other", Parent: "root"}}}}}}
	p := NewSubagentPicker(snapshot, "root", "")
	stop := Key{Typ: KeyRune, Runes: []rune("s")}
	yes := Key{Typ: KeyRune, Runes: []rune("y")}
	id, handled := p.HandleStopKey(stop)
	require.True(t, handled)
	require.Empty(t, id)
	require.Empty(t, p.StopTarget, "root cannot stop")
	p.Navigate(KeyDown)
	id, _ = p.HandleStopKey(stop)
	require.Empty(t, id)
	require.Equal(t, subagent.NodeID("child"), p.StopTarget)
	require.Contains(t, strings.Join(p.Render(120, 12), "\n"), "Stop and drain subtree?")
	id, _ = p.HandleStopKey(Key{Typ: KeyEsc})
	require.Empty(t, id)
	require.Empty(t, p.StopTarget)
	p.HandleStopKey(stop)
	p.Navigate(KeyDown)
	id, _ = p.HandleStopKey(yes)
	require.Equal(t, subagent.NodeID("child"), id, "confirmation never retargets")
	p.HandleStopKey(stop)
	p.Update(subagent.Snapshot{})
	id, _ = p.HandleStopKey(yes)
	require.Empty(t, id, "removed target is not stopped")
}

func TestSubagentPickerUnavailableControlsAndPinnedIdentity(t *testing.T) {
	snapshot := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", SessionID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-unique", Name: "\x1b[31mWorker\nname", Parent: "root"}}}}}}
	p := NewSubagentPicker(snapshot, "root", "")
	p.Navigate(KeyDown)
	p.HandleStopKey(Key{Typ: KeyRune, Runes: []rune("s")})
	snapshot.Nodes[0].Children[0].Node.Name = "renamed"
	p.Update(snapshot)
	view := strings.Join(p.Render(100, 12), "\n")
	require.Contains(t, view, "Worker name (child-unique)")
	require.NotContains(t, p.stopIdentity, "\x1b")
	p.HandleStopKey(Key{Typ: KeyEsc})
	p.PolicyUnavailable, p.StopUnavailable = true, true
	require.False(t, p.HandleActionKey(Key{Typ: KeyRune, Runes: []rune("u")}))
	target, handled := p.HandleStopKey(Key{Typ: KeyRune, Runes: []rune("s")})
	require.True(t, handled)
	require.Empty(t, target)
	require.Empty(t, p.StopTarget)
	view = strings.Join(p.Render(140, 12), "\n")
	require.Contains(t, view, "Use subagents: unavailable")
	require.Contains(t, view, "Stop subtree: unavailable")
	require.Equal(t, subagent.NodeID("child-unique"), p.selected)
}
