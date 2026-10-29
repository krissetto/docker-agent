package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestExpandedWorkPreservesIncomingSenderReferenceTarget(t *testing.T) {
	ar := animation.NewRuntime()
	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "canonical-node", SessionID: "child-session", Agent: "worker", Name: "Worker"}}}})
	m := newModel(ar, 72, 18, &service.SessionState{}, index)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(4, 3)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	input := session.UserMessage("a response from the worker")
	input.InputOrigin, input.InputMode, input.SenderID, input.SenderName = session.InputOriginAgent, "steer", "child-session", "worker"
	m.addMessage(types.Input(input))
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "ref-target", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}, tools.Tool{Name: "shell"}, types.ToolStatusCompleted))
	m.syncWorkProjection()
	block := firstWorkBlock(m)
	require.NotNil(t, block)
	block.SetExpandedImmediately(true)
	m.ensureAllItemsRendered()

	line, col := -1, -1
	for _, row := range block.Rows() {
		if row.SourceIndex != 1 {
			continue
		}
		for local := row.Start; local < row.End; local++ {
			global := m.workRangeStart(1) + local
			plain := ansi.Strip(m.renderedLine(global))
			if offset := strings.Index(plain, "Worker (canon)"); offset >= 0 {
				line, col = global, ansi.StringWidth(plain[:offset])+ansi.StringWidth("From ")
				break
			}
		}
	}
	require.GreaterOrEqual(t, line, 0, "the expanded Work item retains incoming attribution")
	// The sender-name run stays a navigation target after the Work inset is applied.
	ref, ok := m.InputReferenceAt(m.xPos+col, m.yPos+line-m.scrollOffset)
	require.True(t, ok)
	require.Equal(t, lifecycle.InputReferenceNode, ref.Kind)
	require.Equal(t, "canonical-node", ref.ID)
	_, ok = m.InputReferenceAt(m.xPos+col+ansi.StringWidth("Worker"), m.yPos+line-m.scrollOffset)
	require.False(t, ok, "the neutral identity suffix is not linked navigation")
}
