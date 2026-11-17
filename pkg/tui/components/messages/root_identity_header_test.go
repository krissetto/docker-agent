package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
)

// The session root is addressed as "root:<sessionID>" for routing only; its header
// shows a single bare "root" in both collapsed and expanded states.
func TestRootSenderHeaderShowsBareRootInCollapsedAndExpandedStates(t *testing.T) {
	const rootID = "root:parent-full-session-id"
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.NodeID(rootID), Agent: "director", Name: "director"}}}}
	index := subagentindex.New()
	index.Reset(tree)
	m := newModel(animation.NewRuntime(), 100, 100, &service.SessionState{}, index)
	t.Cleanup(m.StopAnimations)
	sess := session.New()
	sess.SetSubagentTree(&tree)
	input := session.UserMessage("root body")
	input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginAgent, "turn", rootID, "director"
	sess.AddMessage(input)
	m.AddInputMessage(input, 0)
	m.SetSize(100, 100)
	m.SetPosition(0, 0)
	m.ensureAllItemsRendered()
	m.View()
	collapsed := ansi.Strip(m.renderedLines[0])
	require.Contains(t, collapsed, "✓ From root sent a message >")
	require.NotContains(t, collapsed, "root:")
	require.NotContains(t, collapsed, "director")
	clickReplyChevron(t, m, ">")
	m.ensureAllItemsRendered()
	m.View()
	expanded := ansi.Strip(m.renderedLines[0])
	require.Contains(t, expanded, "✓ From root sent a message v")
	require.NotContains(t, expanded, "root:")
	require.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "root body")
}
