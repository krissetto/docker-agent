package shell

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type shellModel struct {
	base         *toolcommon.Base
	message      *types.Message
	disclosure   agentmessage.Disclosure
	width        int
	extractCmd   func(string) string
	sessionState service.SessionStateReader
}

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	extractCmd := toolcommon.ExtractField(func(a builtinshell.RunShellArgs) string { return a.Cmd })
	base := toolcommon.NewBase(ar, msg, sessionState, func(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, height int) string {
		return renderShell(msg, s, sessionState, width, height, extractCmd)
	})
	return &shellModel{base: base, message: msg, disclosure: agentmessage.New(ar), width: 80, extractCmd: extractCmd, sessionState: sessionState}
}

func (m *shellModel) Init() tea.Cmd { return m.base.Init() }

func (m *shellModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.base.Update(msg)
	if tick, ok := msg.(animation.TickMsg); ok {
		m.disclosure.Tick(tick)
	}
	return m, cmd
}

func (m *shellModel) SetSize(width, height int) tea.Cmd {
	if m.width != width {
		m.disclosure.Settle()
	}
	m.width = width
	return m.base.SetSize(width, height)
}

func (m *shellModel) View() string {
	header := m.header()
	if !m.disclosure.Visible() {
		return header
	}
	return m.disclosure.Render(header, m.details(), m.width, false)
}

func (m *shellModel) ExpandedView() string { return m.View() }
func (m *shellModel) IsExpanded() bool     { return m.disclosure.Expanded() }
func (m *shellModel) SetExpanded(expanded bool) {
	if m.disclosure.Expanded() != expanded {
		m.disclosure.Toggle()
	}
	m.disclosure.Settle()
}

func (m *shellModel) PreserveExpansionOnParentCollapse() bool { return true }
func (m *shellModel) IsToggleLine(line int) bool              { return line == 0 }
func (m *shellModel) IsToggleAt(line, col int) bool {
	return agentmessage.HeaderAt(m.header(), line, col, m.width)
}
func (m *shellModel) Toggle()                  { m.disclosure.Toggle() }
func (m *shellModel) NeedsTick() bool          { return m.disclosure.NeedsTick() }
func (m *shellModel) StopAnimation()           { m.disclosure.Settle(); m.base.StopAnimation() }
func (m *shellModel) ResumeAnimation() tea.Cmd { return m.base.ResumeAnimation() }

func (m *shellModel) CollapsedView() string { return m.summary() }

func (m *shellModel) summary() string {
	return m.renderHeader(true)
}

func (m *shellModel) header() string {
	return m.renderHeader(!m.disclosure.Expanded() && !m.disclosure.Visible())
}

func (m *shellModel) renderHeader(showPreview bool) string {
	icon := m.base.StatusIcon()
	if m.message.StartedAt != nil && (m.message.ToolStatus == types.ToolStatusPending || m.message.ToolStatus == types.ToolStatusRunning) {
		elapsed := time.Since(*m.message.StartedAt).Truncate(time.Second)
		if elapsed >= time.Second {
			icon += " " + styles.ToolMessageStyle.Render("("+elapsed.String()+")")
		}
	}
	name := m.message.ToolDefinition.DisplayName()
	if name == "" {
		name = "shell"
	}
	line := icon + styles.ToolName.Render(name)
	if showPreview {
		if preview, _ := shellPreview(m.command()); preview != "" {
			available := max(m.width-ansi.StringWidth(line)-1, 0)
			preview = ansi.Truncate(preview, available, "…")
			if preview != "" {
				line += " " + styles.ToolMessageStyle.Render(preview)
			}
		}
	}
	return ansi.Truncate(line, max(m.width, 0), "")
}

func (m *shellModel) command() string {
	arguments := m.message.ToolCall.Function.Arguments
	command := m.extractCmd(arguments)
	if command == "" && m.message.ToolCall.Function.Name != builtinshell.ToolNameShell {
		command = compactArguments(arguments)
	}
	return command
}

func shellPreview(command string) (string, int) {
	command = strings.ReplaceAll(command, "\r\n", "\n")
	command = strings.ReplaceAll(command, "\r", "\n")
	lines := strings.Split(command, "\n")
	first := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#!") {
			continue
		}
		first = i
		break
	}
	if first < 0 {
		return "", 0
	}
	remaining := 0
	for _, line := range lines[first+1:] {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#!") {
			remaining++
		}
	}
	preview := strings.TrimSpace(lines[first])
	if remaining > 0 {
		preview += " … (+" + strconv.Itoa(remaining) + " lines)"
	}
	return preview, remaining
}

func (m *shellModel) details() string {
	width := max(agentmessage.InnerWidth(m.width), 1)
	var sections []string
	if command := m.command(); command != "" {
		commandLines := toolcommon.WrapTextWithIndent("$ "+command, max(width-4, 1), max(width-4, 1))
		sections = append(sections, styles.MutedStyle.Render("Command")+"\n"+styles.ToolCallArgs.Render(commandLines))
	}
	if m.message.Content != "" && (m.sessionState == nil || !m.sessionState.HideToolResults()) {
		outputStyle := styles.ToolMessageStyle
		if m.message.ToolStatus == types.ToolStatusError {
			outputStyle = styles.ToolErrorMessageStyle
		}
		output := toolcommon.WrapTextWithIndent(normalizeShellOutput(m.message.Content), max(width-2, 1), max(width-2, 1))
		if output != "" {
			sections = append(sections, styles.MutedStyle.Render("Output")+"\n"+outputStyle.Render(output))
		}
	}
	return agentmessage.Body(strings.Join(sections, "\n\n"), m.width, false, "")
}

func renderShell(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int, extractCmd func(string) string) string {
	arg := ""
	if msg.ToolCall.Function.Arguments != "" {
		arg = extractCmd(msg.ToolCall.Function.Arguments)
		if arg == "" && msg.ToolCall.Function.Name != builtinshell.ToolNameShell {
			arg = compactArguments(msg.ToolCall.Function.Arguments)
		}
	}

	result := ""
	if msg.Content != "" {
		result = formatShellOutput(msg.Content, width)
	}

	return toolcommon.RenderTool(msg, s, arg, result, width, sessionState.HideToolResults())
}

func formatShellOutput(output string, width int) string {
	output = normalizeShellOutput(output)
	if output == "" {
		return ""
	}
	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	return strings.Join(toolcommon.WrapLines(output, availableWidth), "\n")
}

func normalizeShellOutput(output string) string {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.ReplaceAll(output, "\r", "\n")
	return strings.TrimRight(output, "\n")
}

func compactArguments(arguments string) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(arguments)); err == nil {
		return compact.String()
	}
	return strings.TrimSpace(arguments)
}
