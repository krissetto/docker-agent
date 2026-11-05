package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// maxBodyRows bounds the expanded card: Body's leading and bottom padding rows
// are part of the card, leaving room for the content window and footer hint.
const (
	maxBodyRows  = 12
	bodyPadRows  = 2
	bodyLeadRows = 1
	hintRows     = 1
)

// Keep the same space available to the command preview regardless of status,
// elapsed time, error label, or whether the disclosure control is visible.
const (
	headerIconWidth    = 3
	headerStatusSlot   = 8
	headerControlWidth = 2
)

type shellModel struct {
	ar         *animation.Runtime
	base       *toolcommon.Base
	message    *types.Message
	disclosure agentmessage.Disclosure
	bodyScroll *scrollview.Model
	// Render-time hover reapplication carries no column; reuse the last positional hover on the same row.
	hoverBodyLine  int
	hoverBodyCol   int
	hoverBodyKnown bool
	width          int
	extractCmd     func(string) string
	sessionState   service.SessionStateReader
	hovered        bool
	bodyOffset     int

	bodyArgs   string
	bodyOutput string
	bodyWidth  int
	bodyTheme  uint64
	bodyHidden bool
	bodyStatus types.ToolStatus
	bodyLines  []string

	commandArgs  string
	commandValue string
	commandValid bool

	preservedScroll    int
	preservedAtBottom  bool
	preservedScrollSet bool
}

func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	extractCmd := toolcommon.ExtractField(func(a builtinshell.RunShellArgs) string { return a.Cmd })
	base := toolcommon.NewBase(ar, msg, sessionState, func(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, height int) string {
		return renderShell(msg, s, sessionState, width, height, extractCmd)
	})
	return &shellModel{ar: ar, base: base, message: msg, disclosure: agentmessage.New(ar), width: 80, extractCmd: extractCmd, sessionState: sessionState}
}

func (m *shellModel) Init() tea.Cmd { return m.base.Init() }

func (m *shellModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	_, cmd := m.base.Update(msg)
	if tick, ok := msg.(animation.TickMsg); ok {
		m.disclosure.Tick(tick)
		if m.bodyScroll != nil {
			m.bodyScroll.UpdateAnimation(tick)
		}
	}
	return m, cmd
}

func (m *shellModel) SetSize(width, height int) tea.Cmd {
	if m.width != width {
		m.disclosure.Settle()
		m.bodyOffset = 0
		m.bodyLines = nil
	}
	m.width = width
	return m.base.SetSize(width, height)
}

func (m *shellModel) View() string {
	if m.width < 12 {
		return ansi.Truncate(m.header(), max(m.width, 0), "")
	}
	if !m.disclosure.Visible() {
		return m.header()
	}
	body := m.details()
	header := m.header()
	return m.disclosure.Render(header, body, m.width, false)
}

func (m *shellModel) ExpandedView() string { return m.View() }
func (m *shellModel) IsExpanded() bool     { return m.disclosure.Expanded() }
func (m *shellModel) PreserveBodyScrollFrom(previous any) {
	view, ok := previous.(interface {
		BodyScrollState() (offset int, atBottom, ok bool)
	})
	if !ok {
		return
	}
	offset, atBottom, exists := view.BodyScrollState()
	if !exists {
		return
	}
	m.preservedScroll, m.preservedAtBottom, m.preservedScrollSet = offset, atBottom, true
}

func (m *shellModel) BodyScrollState() (offset int, atBottom, ok bool) {
	if !m.disclosure.Expanded() {
		return 0, false, false
	}
	lines := m.detailsLines()
	return m.bodyOffset, m.bodyOffset >= shellBodyMaxOffset(len(lines)), true
}

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
func (m *shellModel) Toggle() { m.disclosure.Toggle() }

// SetHoveredAt emphasises the header. Body scrollbar reveal needs a column, so
// it goes through SetHoveredPositionAt; line-only hover clears any body pointer.
func (m *shellModel) SetHoveredAt(line int, hovered bool) bool {
	return m.setHover(line, -1, hovered, false)
}

// SetHoveredPositionAt reveals the body scrollbar when the card-local column
// nears the gutter, using the shared scrollview proximity rule.
func (m *shellModel) SetHoveredPositionAt(line, col int, hovered bool) bool {
	return m.setHover(line, col, hovered, true)
}

func (m *shellModel) setHover(line, col int, hovered, positional bool) bool {
	header := hovered && line == 0
	changed := m.hovered != header
	m.hovered = header
	body := hovered && line != 0
	switch {
	case !body:
		m.hoverBodyKnown = false
	case positional:
		m.hoverBodyLine, m.hoverBodyCol, m.hoverBodyKnown = line, col, true
	case m.hoverBodyKnown && line == m.hoverBodyLine:
		col, positional = m.hoverBodyCol, true
	default:
		m.hoverBodyKnown = false
	}
	x, y := -1, -1
	if positional && body && m.width >= 12 && m.disclosure.Expanded() && m.disclosure.Visible() && !m.disclosure.NeedsTick() {
		bodyRows := len(m.detailsLines())
		first, last := shellBodyOutputRows(bodyRows)
		if line >= first && line <= last && shellBodyMaxOffset(bodyRows) > 0 {
			x, y = col-shellCardBodyLeft(), line-first
		}
	}
	if m.bodyScroll == nil && x < 0 {
		return changed
	}
	return m.scroll().SetScrollbarPointer(x, y) || changed
}

// MouseAt routes the expanded body's scrollbar gutter to its scrollview: a left
// press there starts a thumb drag, and an active drag keeps capturing motion and
// release even outside the body. Coordinates are card-local, as for WheelAt.
// Outside a drag, only a press on the gutter is claimed, so body text still
// reaches the transcript.
func (m *shellModel) MouseAt(line, col int, msg tea.MouseMsg) (bool, tea.Cmd) {
	dragging := m.IsBodyDragging()
	if !dragging {
		if _, press := msg.(tea.MouseClickMsg); !press || m.width < 12 || !m.disclosure.Expanded() || !m.disclosure.Visible() || m.disclosure.NeedsTick() {
			return false, nil
		}
	}
	bodyRows := len(m.detailsLines())
	first, last := shellBodyOutputRows(bodyRows)
	x, y := col-shellCardBodyLeft(), line-first
	if !dragging && (shellBodyMaxOffset(bodyRows) == 0 || line < first || line > last || x != m.scroll().ScrollbarX()) {
		return false, nil
	}
	scroll := m.scroll()
	_, cmd := scroll.UpdateMouse(shellLocalMouse(msg, x, y))
	m.bodyOffset = scroll.ScrollOffset()
	return true, cmd
}

// IsBodyDragging reports whether the body scrollbar thumb holds the pointer.
func (m *shellModel) IsBodyDragging() bool { return m.bodyScroll != nil && m.bodyScroll.IsDragging() }

// shellLocalMouse restates a mouse message in the body scrollview's own coordinates.
func shellLocalMouse(msg tea.MouseMsg, x, y int) tea.MouseMsg {
	button := msg.Mouse().Button
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg{X: x, Y: y, Button: button}
	case tea.MouseMotionMsg:
		return tea.MouseMotionMsg{X: x, Y: y, Button: button}
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg{X: x, Y: y, Button: button}
	}
	return msg
}

// cardInnerWidth is the usable width inside the shell card frame.
func cardInnerWidth(width int) int { return max(agentmessage.InnerWidth(width), 1) }

// shellCardBodyLeft is the card column where body rows begin, after the border and padding.
func shellCardBodyLeft() int {
	return styles.UserMessageStyle.GetBorderLeftSize() + styles.UserMessageStyle.GetPaddingLeft()
}

// WheelAt consumes wheel over the visible output rows of a scrollable body, so
// the transcript does not scroll under it, including at the body boundaries.
// Header, padding, and footer hint rows stay with the transcript.
// Coordinates are local to the shell card.
func (m *shellModel) WheelAt(line, col, delta int) bool {
	if delta == 0 || m.width < 12 || !m.disclosure.Expanded() || !m.disclosure.Visible() || m.disclosure.NeedsTick() || col < 0 || col >= m.width {
		return false
	}
	bodyRows := len(m.detailsLines())
	maxOffset := shellBodyMaxOffset(bodyRows)
	first, last := shellBodyOutputRows(bodyRows)
	if maxOffset == 0 || line < first || line > last {
		return false
	}
	if delta > 0 {
		m.bodyOffset = min(maxOffset, saturatingAdd(m.bodyOffset, delta))
	} else {
		m.bodyOffset = max(0, saturatingAdd(m.bodyOffset, delta))
	}
	return true
}

// shellBodyFits reports whether every body row fits without a scroll window.
func shellBodyFits(rows int) bool {
	return rows <= maxBodyRows-bodyPadRows
}

func shellBodyVisibleRows(rows int) int {
	if shellBodyFits(rows) {
		return rows
	}
	return maxBodyRows - bodyPadRows - hintRows
}

func shellBodyMaxOffset(rows int) int {
	return max(0, rows-shellBodyVisibleRows(rows))
}

// shellBodyOutputRows returns the first and last card-local rows that show body output.
func shellBodyOutputRows(rows int) (first, last int) {
	first = 1 + bodyLeadRows
	return first, first + shellBodyVisibleRows(rows) - 1
}

// shellBodyCardRows counts card rows below the header, including padding and the footer hint.
func shellBodyCardRows(rows int) int {
	n := shellBodyVisibleRows(rows) + bodyPadRows
	if !shellBodyFits(rows) {
		n += hintRows
	}
	return n
}

func saturatingAdd(a, b int) int {
	if b > 0 && a > int(^uint(0)>>1)-b {
		return int(^uint(0) >> 1)
	}
	if b < 0 && a < -int(^uint(0)>>1)-1-b {
		return -int(^uint(0)>>1) - 1
	}
	return a + b
}

func (m *shellModel) NeedsTick() bool {
	return m.disclosure.NeedsTick() || (m.bodyScroll != nil && m.bodyScroll.AnimationActive())
}

func (m *shellModel) StopAnimation() {
	m.disclosure.Settle()
	if m.bodyScroll != nil {
		m.bodyScroll.StopAnimation()
	}
	m.base.StopAnimation()
}

func (m *shellModel) ResumeAnimation() tea.Cmd {
	if m.bodyScroll != nil {
		m.bodyScroll.SetScrollbarActive(true)
	}
	return m.base.ResumeAnimation()
}

// scroll allocates the body scrollview on first body render, so collapsed shells stay lightweight.
func (m *shellModel) scroll() *scrollview.Model {
	if m.bodyScroll == nil {
		m.bodyScroll = scrollview.New(
			scrollview.WithAnimationRuntime(m.ar),
			scrollview.WithKeyMap(nil),
			scrollview.WithReserveScrollbarSpace(true),
		)
	}
	return m.bodyScroll
}
func (m *shellModel) CollapsedView() string { return m.summary() }
func (m *shellModel) summary() string       { return m.renderHeader(true) }
func (m *shellModel) header() string {
	return m.renderHeader(!m.disclosure.Expanded() || m.bodyOffset > 0)
}

func (m *shellModel) renderHeader(showPreview bool) string {
	failure, failed := shellFailure(m.message.Content, m.message.ToolStatus)
	icon := m.base.StatusIcon()
	if failed {
		icon = styles.ToolErrorIcon.Render("✗")
	}
	icon = ansi.Truncate(icon, headerIconWidth, "")
	status := ""
	if failed {
		status = styles.ToolErrorMessageStyle.Render(failure)
	} else if m.message.ToolStatus == types.ToolStatusPending || m.message.ToolStatus == types.ToolStatusRunning {
		if m.message.StartedAt != nil {
			elapsed := time.Since(*m.message.StartedAt).Truncate(time.Second)
			if elapsed >= time.Second {
				status = styles.ToolMessageStyle.Render(compactElapsed(elapsed))
			}
		}
	}
	name := m.message.ToolDefinition.DisplayName()
	if name == "" {
		name = "shell"
	}
	line := icon
	if iconWidth := ansi.StringWidth(icon); iconWidth < headerIconWidth {
		line += strings.Repeat(" ", headerIconWidth-iconWidth)
	}
	nameStyle := styles.ToolName.PaddingLeft(0)
	line += " " + nameStyle.Render(name)
	previewWidth := max(m.width-headerIconWidth-1-ansi.StringWidth(nameStyle.Render(name))-1-headerStatusSlot-1-headerControlWidth, 0)
	preview := ""
	if showPreview {
		preview, _ = shellPreviewSanitized(m.command())
	}
	shownPreview := truncateShellPreview(preview, previewWidth)
	if shownPreview != "" {
		previewStyle := styles.ToolMessageStyle
		if m.hovered {
			previewStyle = styles.ToolDescription
		}
		line += " " + previewStyle.Render(shownPreview)
	}
	line += strings.Repeat(" ", max(0, previewWidth-ansi.StringWidth(shownPreview)))
	if status != "" {
		status = ansi.Truncate(status, headerStatusSlot, "…")
		line += " " + strings.Repeat(" ", max(0, headerStatusSlot-ansi.StringWidth(status))) + status
	} else {
		line += strings.Repeat(" ", headerStatusSlot)
	}
	line += " " + agentmessage.Control(m.disclosure.Chevron())
	return ansi.Truncate(line, max(m.width, 0), "")
}

func truncateShellPreview(preview string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(preview) <= width {
		return preview
	}
	if countAt := strings.LastIndex(preview, " … (+"); countAt >= 0 {
		suffix := preview[countAt:]
		if ansi.StringWidth(suffix) <= width {
			prefix := ansi.Truncate(preview[:countAt], width-ansi.StringWidth(suffix), "")
			return strings.TrimRight(prefix, " ") + suffix
		}
	}
	return ansi.Truncate(preview, width, "…")
}

func compactElapsed(elapsed time.Duration) string {
	return elapsed.Truncate(time.Second).String()
}

func (m *shellModel) command() string {
	arguments := m.message.ToolCall.Function.Arguments
	if m.commandValid && m.commandArgs == arguments {
		return m.commandValue
	}
	command := m.extractCmd(arguments)
	if command == "" {
		command = partialCommand(arguments)
	}
	if command == "" && m.message.ToolCall.Function.Name != builtinshell.ToolNameShell {
		command = compactArguments(arguments)
	}
	m.commandArgs = arguments
	m.commandValue = sanitizeTerminalText(command)
	m.commandValid = true
	return m.commandValue
}

func partialCommand(arguments string) string {
	for _, key := range []string{`"cmd"`, `"command"`} {
		index := strings.Index(arguments, key)
		if index < 0 {
			continue
		}
		value := strings.TrimLeftFunc(arguments[index+len(key):], unicode.IsSpace)
		if !strings.HasPrefix(value, ":") {
			continue
		}
		value = strings.TrimLeftFunc(value[1:], unicode.IsSpace)
		if !strings.HasPrefix(value, `"`) {
			continue
		}
		value = value[1:]
		var raw strings.Builder
		escaped := false
		for _, r := range value {
			if r == '"' && !escaped {
				break
			}
			raw.WriteRune(r)
			if r == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		fragment := raw.String()
		var decoded string
		if json.Unmarshal([]byte(`"`+fragment+`"`), &decoded) == nil {
			return decoded
		}
		return strings.ReplaceAll(fragment, `\\"`, `"`)
	}
	return ""
}

func shellPreview(command string) (string, int) {
	return shellPreviewSanitized(sanitizeTerminalText(command))
}

func shellPreviewSanitized(command string) (string, int) {
	command = strings.ReplaceAll(command, "\r\n", "\n")
	command = strings.ReplaceAll(command, "\r", "\n")
	command = strings.ReplaceAll(command, "\t", "    ")
	lines := strings.Split(command, "\n")
	first := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(ansi.Strip(line))
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
		trimmed := strings.TrimSpace(ansi.Strip(line))
		if trimmed != "" && !strings.HasPrefix(trimmed, "#!") {
			remaining++
		}
	}
	preview := strings.TrimSpace(lines[first])
	if remaining > 0 {
		preview += " … (+" + strconv.Itoa(remaining) + " lines)"
	}
	return preview, remaining
}

// details windows the content rows through the shared scrollview so the card
// gets the same scrollbar reveal and edge fade as other scroll surfaces.
func (m *shellModel) details() string {
	if m.width < 12 {
		return ""
	}
	lines := m.detailsLines()
	if len(lines) == 0 {
		return ""
	}
	inner := cardInnerWidth(m.width)
	visibleBodyRows := shellBodyVisibleRows(len(lines))
	start := min(m.bodyOffset, shellBodyMaxOffset(len(lines)))
	bodyScroll := m.scroll()
	bodyScroll.SetSize(inner, visibleBodyRows)
	bodyScroll.SetContent(lines, len(lines))
	bodyScroll.SetScrollOffset(start)
	rows := strings.Split(bodyScroll.ViewWithPaddedContentAndPadding(lines, 0, 0), "\n")
	if !shellBodyFits(len(lines)) {
		remaining := len(lines) - (start + visibleBodyRows)
		hintText := "end of output"
		if remaining > 0 {
			hintText = fmt.Sprintf("… %d more lines · scroll", remaining)
		}
		hint := ansi.Truncate(styles.MutedStyle.Render(hintText), inner, "…")
		rows = append(rows, hint+strings.Repeat(" ", max(0, inner-ansi.StringWidth(hint))))
	}
	return agentmessage.Body(strings.Join(rows, "\n"), m.width, false, "")
}

func styleCommandLines(command string) string {
	lines := strings.Split(command, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "$ ") {
			lines[i] = styles.MutedStyle.Render("$ ") + styles.ToolDescription.Render(line[2:])
		} else {
			lines[i] = styles.ToolDescription.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m *shellModel) detailsLines() []string {
	hideResults := m.sessionState != nil && m.sessionState.HideToolResults()
	arguments := m.message.ToolCall.Function.Arguments
	output := m.message.Content
	generation := styles.ThemeGeneration()
	if m.bodyLines != nil && m.bodyArgs == arguments && m.bodyOutput == output && m.bodyWidth == m.width && m.bodyTheme == generation && m.bodyHidden == hideResults && m.bodyStatus == m.message.ToolStatus {
		return m.bodyLines
	}
	oldOffset := m.bodyOffset
	wasAtBottom := m.bodyLines != nil && oldOffset >= shellBodyMaxOffset(len(m.bodyLines))
	width := max(agentmessage.InnerWidth(m.width)-m.scroll().ReservedCols(), 1)
	var sections []string
	if command := m.command(); command != "" {
		command = strings.ReplaceAll(command, "\r\n", "\n")
		command = strings.ReplaceAll(command, "\r", "\n")
		commandText := strings.ReplaceAll(command, "\t", "    ")
		if highlighted, ok := highlightedCommand(commandText); ok {
			// Embedded scripts keep their highlighting; the prompt stays muted.
			sections = append(sections, ansi.Hardwrap(styles.MutedStyle.Render("$ ")+highlighted, max(width-4, 1), true))
		} else {
			commandLines := toolcommon.WrapTextWithIndent("$ "+commandText, max(width-4, 1), max(width-4, 1))
			sections = append(sections, styleCommandLines(commandLines))
		}
	}
	if output != "" && !hideResults {
		outputStyle := styles.ToolMessageStyle
		if _, failed := shellFailure(output, m.message.ToolStatus); failed {
			outputStyle = styles.ToolErrorMessageStyle
		}
		normalized := normalizeShellOutput(output)
		output = strings.Join(toolcommon.WrapLines(strings.ReplaceAll(normalized, "\t", "    "), max(width-2, 1)), "\n")
		if output != "" {
			divider := styles.MutedStyle.Render(strings.Repeat("─", max(width-2, 1)))
			sections = append(sections, divider+"\n"+outputStyle.Render(output))
		}
	}
	body := strings.Join(sections, "\n\n")
	if body == "" {
		m.bodyLines = nil
	} else {
		m.bodyLines = strings.Split(body, "\n")
	}
	m.bodyArgs = arguments
	m.bodyOutput = m.message.Content
	m.bodyWidth = m.width
	m.bodyTheme = generation
	m.bodyHidden = hideResults
	m.bodyStatus = m.message.ToolStatus
	if m.preservedScrollSet {
		if m.preservedAtBottom {
			m.bodyOffset = shellBodyMaxOffset(len(m.bodyLines))
		} else {
			m.bodyOffset = min(m.preservedScroll, shellBodyMaxOffset(len(m.bodyLines)))
		}
		m.preservedScrollSet = false
	} else if wasAtBottom {
		m.bodyOffset = shellBodyMaxOffset(len(m.bodyLines))
	} else {
		m.bodyOffset = min(oldOffset, shellBodyMaxOffset(len(m.bodyLines)))
	}
	return m.bodyLines
}

func shellOutputPayload(content string) string {
	if strings.HasPrefix(content, "[shell-hint] ") {
		if _, payload, found := strings.Cut(content, "\n\n"); found {
			return payload
		}
	}
	return content
}

func shellFailure(content string, status types.ToolStatus) (string, bool) {
	if strings.HasPrefix(content, "Command timed out after ") {
		return "timeout", true
	}
	const marker = "Error executing command: exit status "
	if payload := shellOutputPayload(content); strings.HasPrefix(payload, marker) {
		rest := payload[len(marker):]
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 0 {
			if code, err := strconv.Atoi(rest[:end]); err == nil && code != 0 {
				return "exit " + strconv.Itoa(code), true
			}
		}
		return "failed", true
	}
	if strings.HasPrefix(shellOutputPayload(content), "Error executing command:") {
		return "failed", true
	}
	if status == types.ToolStatusError {
		return "error", true
	}
	return "", false
}

// sanitizeTerminalText keeps SGR color but drops terminal control strings and
// cursor/window commands so tool output cannot alter the user's terminal.
func sanitizeTerminalText(text string) string {
	needsSanitize := !utf8.ValidString(text) || strings.IndexFunc(text, func(r rune) bool {
		return r == '\x1b' || unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' || r >= 0x80 && r <= 0x9f
	}) >= 0
	if !needsSanitize {
		return text
	}
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var out strings.Builder
	out.Grow(len(text))
	var state byte
	for rest := text; rest != ""; {
		if rest[0] == '\x1b' {
			sequence, _, consumed, next := ansi.DecodeSequence(rest, state, parser)
			if consumed <= 0 {
				consumed = 1
			}
			if ansi.HasCsiPrefix(sequence) && parser.Command() == 'm' {
				out.WriteString(sequence)
			}
			state, rest = next, rest[consumed:]
			continue
		}
		r, size := utf8.DecodeRuneInString(rest)
		if size == 0 {
			break
		}
		if r == utf8.RuneError && size == 1 {
			out.WriteRune(utf8.RuneError)
			rest = rest[1:]
			state = 0
			continue
		}
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsControl(r) && !(r >= 0x80 && r <= 0x9f) {
			out.WriteString(rest[:size])
		}
		rest = rest[size:]
		state = 0
	}
	return out.String()
}

func renderShell(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int, extractCmd func(string) string) string {
	arg := ""
	if msg.ToolCall.Function.Arguments != "" {
		arg = sanitizeTerminalText(extractCmd(msg.ToolCall.Function.Arguments))
		if arg == "" && msg.ToolCall.Function.Name != builtinshell.ToolNameShell {
			arg = sanitizeTerminalText(compactArguments(msg.ToolCall.Function.Arguments))
		}
	}

	result := ""
	if msg.Content != "" {
		result = formatShellOutput(msg.Content, width)
	}

	hideResults := sessionState != nil && sessionState.HideToolResults()
	return toolcommon.RenderTool(msg, s, arg, result, width, hideResults)
}

func formatShellOutput(output string, width int) string {
	output = normalizeShellOutput(output)
	output = strings.ReplaceAll(output, "\t", "    ")
	if output == "" {
		return ""
	}
	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	return strings.Join(toolcommon.WrapLines(output, availableWidth), "\n")
}

func normalizeShellOutput(output string) string {
	output = sanitizeTerminalText(output)
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
