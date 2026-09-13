package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/tool/subagenttool"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestInputIdentitySidebarToolNoticeAndBorderShareLabelColor(t *testing.T) {
	styles.SetAgentOrder([]string{"root", "worker"})
	defer styles.SetAgentOrder(nil)
	node := subagent.Node{ID: "a1b2c", SessionID: "other-session-id", Agent: "worker", Name: "Visible worker", State: subagent.NodeCompleted}
	ref := lifecycle.ResolveInputReference(&subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: node}}}, "", node.SessionID, "stale")
	m := newSubagentTestModel(t)
	m.hoveredSubagent = node.ID
	sidebar := m.subagentLine(node, "", 80)
	assert.Contains(t, ansi.Strip(sidebar), ref.Label())
	assert.Contains(t, sidebar, styles.AgentIdentityStyle(node.Agent, true).Render(node.DisplayName()))
	assert.Equal(t, styles.AgentAccentStyleFor(node.Agent).Render("name"), styles.AgentIdentityStyle(node.Agent, false).Render("name"))
	assert.Equal(t, styles.AgentAccentStyleFor(node.Agent).Foreground(styles.Brighten(styles.AgentAccentStyleFor(node.Agent).GetForeground(), 0.25)).Render("name"), styles.AgentIdentityStyle(node.Agent, true).Render("name"))
	label := agentidentity.Label(ref, 80)
	name := styles.AgentIdentityStyle(node.Agent, false).Render(node.DisplayName())
	suffix := styles.MutedStyle.Render(" (" + ref.DisplayID + ")")
	assert.Equal(t, ansi.SetHyperlink(agentidentity.Link)+name+suffix+ansi.ResetHyperlink(), label)
	input := &types.Message{Type: types.MessageTypeAgentInput, Content: "literal **body**", InputReference: ref}
	border := message.New(animation.NewRuntime(), input, nil).Render(80)
	assert.Contains(t, border, ansi.SetHyperlink(agentidentity.Link))
	assert.Contains(t, border, ansi.ResetHyperlink())
	// The border reapplies its USER background after each inner style reset.
	assert.Contains(t, border, strings.TrimSuffix(name, "\x1b[m"))
	assert.Contains(t, border, strings.TrimSuffix(suffix, "\x1b[m"))
	input.Type = types.MessageTypeRuntimeNotice
	assert.Contains(t, subagenttool.RenderInput(input, 80), label)
	tool := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker"}`}}, tools.Tool{}, types.ToolStatusCompleted)
	tool.Content = `Spawned subagent "Visible worker" (a1b2c).`
	view := subagenttool.NewSpawn(animation.NewRuntime(), tool, &service.SessionState{}, nil, func(subagent.NodeID) lifecycle.InputReference { return ref })
	assert.Contains(t, ansi.Strip(view.View()), ref.Label())
	assert.Contains(t, view.View(), label, "tool uses canonical name color and neutral ID despite display alias")
	assert.NotContains(t, ansi.Strip(border), node.SessionID)
	assert.Equal(t, "┃ "+ref.Label(), strings.TrimSpace(strings.Split(ansi.Strip(border), "\n")[0]))
	assert.NotContains(t, ansi.Strip(border), "━", "identity header has no top rule")
}
