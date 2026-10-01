package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestRoutedVisiblePaneKeepsMessageReferencePresentation(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-canonical-node", SessionID: "child-session", Agent: "worker", Name: "Worker"}}}}
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: &runtime.SubagentTreeEvent{Snapshot: tree}})
	root.Update(messages.RoutedMsg{SessionID: "profile", Inner: &runtime.UserMessageEvent{Message: "raw report", TurnID: "report:unrelated", InputOrigin: session.InputOriginRuntime, SenderID: "child-session", SenderName: "worker", SessionPosition: 100000}})
	page := root.chatPages["profile"]
	frame := page.(chat.SplitPresentation).TranscriptView()
	rect := root.paneGeometry.Panes["profile"]
	for y, line := range strings.Split(frame, "\n") {
		before, _, found := strings.Cut(ansi.Strip(line), "(abcde)")
		if !found {
			continue
		}
		beforeCount := root.ar.ActiveCount()
		page.Update(tea.MouseMotionMsg{X: rect.X + ansi.StringWidth(before), Y: rect.Y + y})
		require.Equal(t, beforeCount+1, root.ar.ActiveCount(), "background ingestion cannot disable visible transcript hover")
		gap := root.paneAreas(rect).gap
		root.Update(tea.MouseMotionMsg{X: gap.X, Y: gap.Y})
		require.Equal(t, beforeCount, root.ar.ActiveCount(), "blank footer gap clears the unfocused pane's reference hover")
		return
	}
	t.Fatalf("completion missing from visible pane: %q", ansi.Strip(frame))
}
