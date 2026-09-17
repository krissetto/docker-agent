package sidebar

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	m.branchSpans = map[subagent.NodeID]branchSpans{node.ID: {idValue: 1, idTarget: 1}}
	m.hoverValues = map[string]hoverValue{"node:" + string(node.ID): {value: 1, target: 1}}
	sidebar := m.subagentLine(node, "", 80)
	assert.Contains(t, ansi.Strip(sidebar), node.DisplayName()+" ✓ ("+ref.DisplayID+")", "sidebar state glyph precedes neutral canonical ID")
	assert.Contains(t, sidebar, neutralSpan(" ("+ref.DisplayID+")", 1, ansi.StringWidth(" ("+ref.DisplayID+")")), "ID retains neutral foreground")
	assert.Contains(t, sidebar, styles.HoverText(styles.AgentIdentityStyle(node.Agent, false).Render(node.DisplayName()), 1, styles.TextPrimary))
	assert.Equal(t, styles.AgentAccentStyleFor(node.Agent).Render("name"), styles.AgentIdentityStyle(node.Agent, false).Render("name"))
	assert.Equal(t, styles.AgentAccentStyleFor(node.Agent).Foreground(styles.Brighten(styles.AgentAccentStyleFor(node.Agent).GetForeground(), 0.25)).Render("name"), styles.AgentIdentityStyle(node.Agent, true).Render("name"))
	label := agentidentity.Label(ref, 80)
	name := styles.AgentIdentityStyle(node.Agent, false).Render(node.DisplayName())
	suffix := styles.MutedStyle.Render(" (" + ref.DisplayID + ")")
	assert.Equal(t, ref.Label(), ansi.Strip(label))
	linked := identityLinkCells(label)
	require.Len(t, linked, ansi.StringWidth(ref.Label()))
	for _, url := range linked {
		assert.Equal(t, agentidentity.Link, url, "name and neutral ID share the full clickable identity range")
	}
	cells := sidebarCells(label)
	nameCells := ansi.StringWidth(node.DisplayName())
	for i, cell := range cells {
		want := styles.MutedStyle.GetForeground()
		if i < nameCells {
			want = styles.AgentIdentityStyle(node.Agent, false).GetForeground()
		}
		assert.Equal(t, color.NRGBAModel.Convert(want), color.NRGBAModel.Convert(cell.fg), "name and ID keep independent semantic colors")
	}
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
	rows := strings.Split(border, "\n")
	prefix := "┏━ "
	header := prefix + ref.Label() + " "
	assert.Equal(t, header+strings.Repeat("━", 80-ansi.StringWidth(header)), ansi.Strip(rows[0]), "canonical identity is embedded in the top border")
	assert.Equal(t, "┃ literal **body**", strings.TrimRight(ansi.Strip(rows[1]), " "), "literal USER body stays below the identity border")
	borderLinks := identityLinkCells(rows[0])
	require.Len(t, borderLinks, 80)
	start := ansi.StringWidth(prefix)
	for x, url := range borderLinks {
		if x >= start && x < start+ansi.StringWidth(ref.Label()) {
			assert.Equal(t, agentidentity.Link, url, "canonical name and neutral ID remain linked at border cell %d", x)
		} else {
			assert.Empty(t, url, "border rules and padding are not identity targets at cell %d", x)
		}
	}
}

func identityLinkCells(line string) []string {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	var active string
	var cells []string
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, parser)
		if n == 0 {
			break
		}
		if payload, ok := strings.CutPrefix(seq, "\x1b]8;"); ok {
			payload = strings.TrimSuffix(strings.TrimSuffix(payload, "\a"), "\x1b\\")
			_, active, _ = strings.Cut(payload, ";")
		}
		for range width {
			cells = append(cells, active)
		}
		state, line = next, line[n:]
	}
	return cells
}
