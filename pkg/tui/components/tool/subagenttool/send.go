package subagenttool

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type sendModel struct {
	*toolcommon.Base

	msg         *types.Message
	disclosure  agentmessage.Disclosure
	width       int
	lookup      NameLookup
	references  []ReferenceLookup
	headerLines int
}

func (m *sendModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.Base.Update(msg)
	if tick, ok := msg.(animation.TickMsg); ok {
		m.disclosure.Tick(tick)
	}
	return m, cmd
}

func (m *sendModel) View() string {
	header := m.Base.View()
	if !m.disclosure.Visible() {
		return header
	}
	params, err := toolcommon.ParseArgs[subagent.SendArgs](m.msg.ToolCall.Function.Arguments)
	if err != nil {
		return header
	}
	body := agentmessage.Body(params.Message, m.width, false, "")
	return m.disclosure.Render(header, body)
}

func (m *sendModel) ExpandedView() string { return m.View() }
func (m *sendModel) CollapsedView() string {
	return m.Base.CollapsedView()
}
func (m *sendModel) IsToggleLine(int) bool { return false }
func (m *sendModel) IsToggleAt(line, col int) bool {
	header := m.Base.View()
	if m.msg.ToolStatus == types.ToolStatusError {
		header = strings.Join(strings.Split(header, "\n")[:m.headerLines], "\n")
	}
	return agentmessage.ToggleAt(header, line, col)
}
func (m *sendModel) Toggle()         { m.disclosure.Toggle() }
func (m *sendModel) NeedsTick() bool { return m.disclosure.NeedsTick() }
func (m *sendModel) StopAnimation()  { m.disclosure.Settle(); m.Base.StopAnimation() }
func (m *sendModel) SetSize(width, height int) tea.Cmd {
	if m.width != width {
		m.disclosure.Settle()
	}
	m.width = width
	return m.Base.SetSize(width, height)
}

func (m *sendModel) IsExpanded() bool { return m.disclosure.Expanded() }
func (m *sendModel) SetExpanded(expanded bool) {
	if expanded != m.disclosure.Expanded() {
		m.disclosure.Toggle()
	}
	m.disclosure.Settle()
}

func (m *sendModel) InputReferenceOnLine(line int) bool {
	m.Base.View()
	return line >= 0 && line < m.headerLines
}

func (m *sendModel) reference(msg *types.Message, to string) lifecycle.InputReference {
	name, id := attribution(msg, to, m.lookup)
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: id, Name: name, Agent: name, DisplayID: subagent.ShortID(id)}
	if to == subagent.ParentAlias {
		return lifecycle.InputReference{Kind: lifecycle.InputReferenceParent, Name: "parent"}
	}
	if len(m.references) > 0 && m.references[0] != nil {
		ref.Agent = m.references[0](subagent.NodeID(id)).Agent
	}
	return ref
}

func (m *sendModel) header(msg *types.Message, s spinner.Spinner, width int) string {
	params, _ := toolcommon.ParseArgs[subagent.SendArgs](msg.ToolCall.Function.Arguments)
	prefix := statusIcon(msg, s) + " " + styles.MutedStyle.Render(verb(msg, "Messaging", "Messaged")) + " "
	return m.disclosure.Header(prefix, m.reference(msg, params.To), "", width)
}
