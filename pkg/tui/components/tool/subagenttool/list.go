package subagenttool

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const (
	maxListAgentsRows        = 50
	maxListAgentsResultSize  = 512 << 10
	maxListAgentSessionTitle = 100
)

type listedAgentRef struct {
	ID           string `json:"id"`
	Agent        string `json:"agent"`
	Name         string `json:"name"`
	SessionTitle string `json:"session_title"`
	State        string `json:"state"`
	ParentID     string `json:"parent_id"`
	Messageable  *bool  `json:"messageable"`
}

type listedAgentsResult struct {
	CurrentAgent string           `json:"current_agent"`
	Self         *listedAgentRef  `json:"self"`
	Parent       *listedAgentRef  `json:"parent"`
	Children     []listedAgentRef `json:"children"`
	Agents       []listedAgentRef `json:"agents"`
	Scope        string           `json:"scope"`
	NextCursor   string           `json:"next_cursor"`
}

type listAgentsModel struct {
	base       *toolcommon.Base
	msg        *types.Message
	disclosure agentmessage.Disclosure
	width      int
	references []ReferenceLookup

	parsedContent string
	parsed        listedAgentsResult
	parseOK       bool
	lineRefs      []lifecycle.InputReference
	headerLines   int
}

// NewList renders the bounded roster returned by list_agents. References are
// supplied by the shared subagent index so only known canonical nodes navigate.
func NewList(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader, references ...ReferenceLookup) layout.Model {
	m := &listAgentsModel{msg: msg, disclosure: agentmessage.New(ar), width: 80, references: references}
	m.base = toolcommon.NewBaseWithCollapsed(ar, msg, sessionState,
		func(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int) string {
			return m.header(msg, s, width, false)
		},
		func(msg *types.Message, s spinner.Spinner, _ service.SessionStateReader, width, _ int) string {
			return m.header(msg, s, width, true)
		},
	)
	return m
}

func (m *listAgentsModel) Init() tea.Cmd { return m.base.Init() }

func (m *listAgentsModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.base.Update(msg)
	if tick, ok := msg.(animation.TickMsg); ok {
		m.disclosure.Tick(tick)
	}
	return m, cmd
}

func (m *listAgentsModel) SetSize(width, height int) tea.Cmd {
	if m.width != width {
		m.disclosure.Settle()
	}
	m.width = width
	return m.base.SetSize(width, height)
}

func (m *listAgentsModel) View() string {
	header := m.base.View()
	m.lineRefs = nil
	m.headerLines = strings.Count(header, "\n") + 1
	if !m.disclosure.Visible() {
		return header
	}
	body := m.details()
	if body == "" {
		return header
	}
	return m.disclosure.Render(header, body, m.width, false)
}

func (m *listAgentsModel) ExpandedView() string  { return m.View() }
func (m *listAgentsModel) CollapsedView() string { return m.base.CollapsedView() }
func (m *listAgentsModel) IsExpanded() bool      { return m.disclosure.Expanded() }
func (m *listAgentsModel) SetExpanded(expanded bool) {
	if m.disclosure.Expanded() != expanded {
		m.disclosure.Toggle()
	}
	m.disclosure.Settle()
}
func (m *listAgentsModel) IsToggleLine(line int) bool { return line >= 0 && line < m.headerLines }
func (m *listAgentsModel) IsToggleAt(line, col int) bool {
	return agentmessage.HeaderAt(m.base.View(), line, col, m.width)
}
func (m *listAgentsModel) Toggle() { m.disclosure.Toggle() }

func (m *listAgentsModel) NeedsTick() bool { return m.disclosure.NeedsTick() }

func (m *listAgentsModel) StopAnimation() {
	m.disclosure.Settle()
	m.base.StopAnimation()
}

func (m *listAgentsModel) ResumeAnimation() tea.Cmd { return m.base.ResumeAnimation() }

func (m *listAgentsModel) PreserveExpansionOnParentCollapse() bool { return true }

func (m *listAgentsModel) InputReferenceForLine(line int) (lifecycle.InputReference, bool) {
	if !m.disclosure.Expanded() || m.disclosure.NeedsTick() {
		return lifecycle.InputReference{}, false
	}
	index := line - m.headerLines
	if index < 0 || index >= len(m.lineRefs) {
		return lifecycle.InputReference{}, false
	}
	ref := m.lineRefs[index]
	return ref, ref.Kind == lifecycle.InputReferenceNode
}

func (m *listAgentsModel) InputReferenceOnLine(line int) bool {
	_, ok := m.InputReferenceForLine(line)
	return ok
}

func (m *listAgentsModel) header(msg *types.Message, s spinner.Spinner, width int, collapsed bool) string {
	result, ok := m.result(msg.Content)
	label := "Listing agents"
	if msg.ToolStatus == types.ToolStatusError {
		label = "Agent listing failed"
	} else if msg.ToolStatus != types.ToolStatusRunning && msg.ToolStatus != types.ToolStatusPending && msg.ToolStatus != types.ToolStatusConfirmation {
		if ok {
			agents := agentsForResult(result)
			label = "Listed " + plural(len(agents), "agent")
			if running := runningAgentCount(agents); running > 0 {
				label += " · " + strconv.Itoa(running) + " running"
			}
			if result.Scope == "tree" {
				label += " in tree"
			}
		} else {
			label = "Agent list unavailable"
		}
	}
	prefix := statusIcon(msg, s) + " " + styles.MutedStyle.Render(label)
	var header string
	if collapsed {
		header = ansi.Hardwrap(prefix, max(width, 1), true)
	} else {
		header = agentmessage.CompactHeader(prefix, lifecycle.InputReference{Kind: lifecycle.InputReferenceUnknown}, "", m.disclosure.Chevron(), width)
	}
	return header
}

func (m *listAgentsModel) result(content string) (listedAgentsResult, bool) {
	if content != m.parsedContent {
		m.parsedContent = content
		m.parsed = listedAgentsResult{}
		trimmed := strings.TrimSpace(content)
		m.parseOK = content != "" && len(content) <= maxListAgentsResultSize && strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(content), &m.parsed) == nil && (m.parsed.Children != nil || (m.parsed.Scope == "tree" && m.parsed.Agents != nil))
	}
	return m.parsed, m.parseOK
}

func (m *listAgentsModel) details() string {
	result, ok := m.result(m.msg.Content)
	if !ok || m.msg.ToolStatus == types.ToolStatusError {
		m.lineRefs = nil
		if m.msg.ToolStatus == types.ToolStatusError {
			return styles.MutedStyle.Render("Agent listing failed")
		}
		return styles.MutedStyle.Render("Agent list unavailable")
	}

	var lines []string
	m.lineRefs = nil
	appendLine := func(line string, ref lifecycle.InputReference) {
		wrapped := strings.Split(line, "\n")
		lines = append(lines, wrapped...)
		for range wrapped {
			m.lineRefs = append(m.lineRefs, ref)
		}
	}
	appendPlain := func(line string) {
		appendLine(ansi.Hardwrap(line, max(m.width, 1), true), lifecycle.InputReference{})
	}

	appendIdentity := func(label string, agent listedAgentRef) {
		name := agent.Name
		if name == "" {
			name = agent.Agent
		}
		if name == "" {
			return
		}
		ref := m.nodeReference(agent, name)
		prefix := styles.MutedStyle.Render(label + " ")
		appendLine(agentidentity.Wrap(prefix, ref, sessionTitleSuffix(agent.SessionTitle), max(m.width, 1)), ref)
	}
	current := result.CurrentAgent
	if result.Self != nil {
		current = ""
		appendIdentity("You:", *result.Self)
	} else if current != "" {
		current = safeLabel(current)
		appendPlain(styles.MutedStyle.Render("You: ") + styles.AgentBadgeStyleFor(current).Render(current))
	}
	if result.Parent != nil {
		appendIdentity("Parent:", *result.Parent)
	}

	agents := agentsForResult(result)
	for _, agent := range agents {
		// Self and parent are already represented as context rows above.
		if (result.Self != nil && agent.ID == result.Self.ID) || (result.Parent != nil && agent.ID == result.Parent.ID) {
			continue
		}
		name := agent.Name
		if name == "" {
			name = agent.Agent
		}
		ref := m.nodeReference(agent, name)
		icon := stateIcon(agent.State, safeLabel(agent.Agent))
		prefix := "  " + icon + " "
		suffix := sessionTitleSuffix(agent.SessionTitle)
		if agent.Messageable != nil && !*agent.Messageable {
			suffix += readOnlyMarker(m.width)
		}
		row := agentidentity.Wrap(prefix, ref, suffix, max(m.width, 1))
		appendLine(row, ref)
	}
	if len(agents) == 0 && result.Self == nil && result.Parent == nil && current == "" {
		appendPlain(styles.MutedStyle.Render("No active subagents"))
	}
	if result.NextCursor != "" {
		appendPlain(styles.MutedStyle.Render("Next page available"))
	}
	return strings.Join(lines, "\n")
}

func agentsForResult(result listedAgentsResult) []listedAgentRef {
	agents := result.Children
	if result.Scope == "tree" && result.Agents != nil {
		agents = result.Agents
	}
	return agents[:min(len(agents), maxListAgentsRows)]
}

func (m *listAgentsModel) nodeReference(agent listedAgentRef, name string) lifecycle.InputReference {
	id := agent.ID
	fallback := lifecycle.InputReference{Kind: lifecycle.InputReferenceUnknown, ID: id, Name: safeLabel(name), Agent: safeLabel(agent.Agent), DisplayID: safeLabel(displayAgentID(id))}
	if len(id) != 5 || !isLowerHexID(id) || len(m.references) == 0 || m.references[0] == nil {
		return fallback
	}
	ref := m.references[0](subagent.NodeID(id))
	if ref.Kind != lifecycle.InputReferenceNode || ref.ID != id {
		return fallback
	}
	if ref.Name == "" {
		ref.Name = fallback.Name
	} else {
		ref.Name = safeLabel(ref.Name)
	}
	if ref.Agent == "" {
		ref.Agent = fallback.Agent
	} else {
		ref.Agent = safeLabel(ref.Agent)
	}
	if ref.DisplayID == "" {
		ref.DisplayID = fallback.DisplayID
	} else {
		ref.DisplayID = safeLabel(ref.DisplayID)
	}
	return ref
}

func stateIcon(state, agent string) string {
	switch subagent.NodeState(state) {
	case subagent.NodeRunning:
		return styles.AgentAccentStyleFor(agent).Render("●")
	case subagent.NodeStarting:
		return styles.AgentAccentStyleFor(agent).Render("·")
	case subagent.NodeFailed:
		return styles.ErrorStyle.Render("✗")
	case subagent.NodeIdle:
		return styles.MutedStyle.Render("○")
	case subagent.NodeCompleted, subagent.NodeStopped:
		return styles.SuccessStyle.Render("✓")
	default:
		return styles.MutedStyle.Render("·")
	}
}

func runningAgentCount(agents []listedAgentRef) int {
	count := 0
	if len(agents) > maxListAgentsRows {
		agents = agents[:maxListAgentsRows]
	}
	for _, agent := range agents {
		if agent.State == string(subagent.NodeRunning) {
			count++
		}
	}
	return count
}

func plural(count int, word string) string {
	if count == 1 {
		return "1 " + word
	}
	return strconv.Itoa(count) + " " + word + "s"
}

func displayAgentID(id string) string {
	if sessionID, ok := strings.CutPrefix(id, "root:"); ok {
		sessionID := []rune(sessionID)
		if len(sessionID) == 0 {
			return "root"
		}
		if len(sessionID) > 8 {
			sessionID = sessionID[len(sessionID)-8:]
		}
		return "root:" + string(sessionID)
	}
	return subagent.ShortID(id)
}

func readOnlyMarker(width int) string {
	switch {
	case width >= 12:
		return styles.MutedStyle.Render(" · 🔒 read-only")
	case width >= 3:
		return styles.MutedStyle.Render(" 🔒")
	case width >= 2:
		return styles.MutedStyle.Render("🔒")
	default:
		return styles.MutedStyle.Render("R")
	}
}

func sessionTitleSuffix(value string) string {
	title := ansi.Truncate(safeLabel(value), maxListAgentSessionTitle, "…")
	if title == "" {
		return ""
	}
	return styles.MutedStyle.Render(" · " + title)
}

func safeLabel(value string) string {
	plain := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, ansi.Strip(value))
	return ansi.Truncate(strings.Join(strings.Fields(plain), " "), 160, "…")
}

func isLowerHexID(id string) bool {
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
