package sidebar

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Reserve the status row independently of YOLO toggles to keep viewport geometry stable.
func (m *model) footerHeight() int {
	if m.height > 0 && (m.sessionState.YoloMode() || m.activeAgentName() != "") {
		return 1
	}
	return 0
}

// Keep compact/two-row shells unchanged and leave the pinned footer at height-1.
func (m *model) footerGapHeight() int {
	if m.mode == ModeVertical && m.height > 2 && m.footerHeight() > 0 {
		return 1
	}
	return 0
}

func (m *model) viewportHeight() int { return max(0, m.height-m.footerHeight()-m.footerGapHeight()) }

// activeModelDetails uses only event-projected metadata, never runtime I/O.
func (m *model) activeModelDetails() runtime.AgentDetails {
	for _, details := range m.availableAgents {
		if details.Name != m.activeAgentName() {
			continue
		}
		if m.agentModel != "" && details.Name == m.currentAgent && details.ModelID != "" {
			ref := details.ModelID
			if details.Provider != "" {
				ref = details.Provider + "/" + ref
			}
			if m.agentModel != ref && m.agentModel != details.Model && m.agentModel != details.Provider+"/"+details.Model {
				return runtime.AgentDetails{}
			}
		}
		return details
	}
	return runtime.AgentDetails{}
}

func (m *model) activeThinkingLabel() string {
	details := m.activeModelDetails()
	control := details.ThinkingControl()
	level := control.Level
	if level == "" {
		switch control.Mode {
		case "default", "off", "auto", "adaptive", "unknown", "unsupported":
			level = control.Mode
		default:
			level = details.Thinking
		}
	}
	if level == "" {
		return ""
	}
	if details.PrimaryThinking != nil {
		level = "primary: " + level
	}
	return styles.MutedStyle.Render("(" + collapsedSingleLine(level) + ")")
}

// ThinkingTarget exposes the exact projected origin without changing its value.
func (m *model) ThinkingTarget() (agentName, modelRef string, enabled bool) {
	details := m.activeModelDetails()
	model, provider := m.activeModelIdentity()
	if provider != "" {
		model = provider + "/" + model
	}
	control := details.ThinkingControl()
	if details.PrimaryThinking != nil {
		model = control.ModelRef
	}
	return m.activeAgentName(), model, control.CanCycle
}

// ThinkingDisplayReference additionally guards the active fallback projection.
func (m *model) ThinkingDisplayReference() string {
	if m.activeModelDetails().PrimaryThinking == nil {
		return ""
	}
	model, provider := m.activeModelIdentity()
	if provider != "" {
		model = provider + "/" + model
	}
	return model
}

func modelFooterSpans(model, thinking string, canCycle bool, budget int) []collapsedSpan {
	model = ansi.Truncate(collapsedSingleLine(model), max(0, budget), "…")
	spans := []collapsedSpan{{text: model, action: ClickModel}}
	x := ansi.StringWidth(model)
	if thinking != "" && x+1+ansi.StringWidth(thinking) <= budget {
		action := ClickNone
		if canCycle {
			action = ClickThinkingLevel
		}
		spans = append(spans, collapsedSpan{text: thinking, x: x + 1, action: action})
	}
	return spans
}

func (m *model) modelSpans(width int) []collapsedSpan {
	info := strings.Split(m.activeModelInfo(max(1, width)), "\n")
	if len(info) == 0 || info[0] == "" {
		return nil
	}
	spans := []collapsedSpan{{text: ansi.Truncate(info[0], max(0, width), "…"), action: ClickModel}}
	provider := ""
	if len(info) > 1 {
		provider = info[1]
	}
	for _, span := range modelFooterSpans(provider, m.activeThinkingLabel(), m.activeModelDetails().ThinkingControl().CanCycle, max(0, width)) {
		span.y = 1
		spans = append(spans, span)
	}
	return spans
}

func (m *model) modelRowView(width, row int) string {
	var model strings.Builder
	column := 0
	for _, span := range m.modelSpans(width) {
		if span.y != row {
			continue
		}
		text := span.text
		switch span.action {
		case ClickModel:
			text = m.hoverText(text, "model")
		case ClickThinkingLevel:
			text = m.hoverText(text, "thinking-level")
		}
		gap := max(0, span.x-column)
		model.WriteString(strings.Repeat(" ", gap))
		model.WriteString(text)
		column += gap + ansi.StringWidth(text)
	}
	return model.String()
}

func (m *model) modelView(width int) string {
	if len(m.modelSpans(width)) == 0 {
		return ""
	}
	return m.modelRowView(width, 0) + "\n" + m.modelRowView(width, 1)
}

func (m *model) modelClick(column, row, width int) ClickResult {
	for _, span := range m.modelSpans(width) {
		if span.y == row && column >= span.x && column < span.x+ansi.StringWidth(span.text) {
			return span.action
		}
	}
	return ClickNone
}

func (m *model) agentIdentityView(width int) string {
	agent := ansi.Truncate(collapsedSingleLine(m.activeAgentName()), max(0, width), "…")
	return styles.AgentIdentityStyle(m.activeAgentName(), false).Render(agent)
}

func (m *model) footerView(width int) string {
	pill := m.yoloIndicator(width)
	return strings.Repeat(" ", max(0, width-ansi.StringWidth(pill))) + pill
}
