package sidebar

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// CollapsedViewModel holds the content of the compact horizontal sidebar.
// Rendering and hit testing share the same bounded two-row layout.
type CollapsedViewModel struct {
	TitleWithStar    string
	WorkingIndicator string
	WorkingDir       string
	Branch           string
	Yolo             string
	AgentIdentity    string
	ModelInfo        string
	Thinking         string
	CanCycleThinking bool
	UsageSummary     string
	InfoLine         string

	TitleAndIndicatorOnOneLine bool
	WdAndUsageOnOneLine        bool
	ContentWidth               int
}

type collapsedSpan struct {
	text   string
	x, y   int
	action ClickResult
}

func collapsedSingleLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(text)
}

// spans reserves the right-hand usage summary before fitting optional metadata.
// Long and multiline titles or model descriptions can never grow the band.
func (vm CollapsedViewModel) spans() []collapsedSpan {
	width := max(0, vm.ContentWidth)
	var spans []collapsedSpan
	appendSpan := func(text string, x, y, budget int, action ClickResult) int {
		text = ansi.Truncate(collapsedSingleLine(text), max(0, budget), "…")
		if text == "" {
			return x
		}
		spans = append(spans, collapsedSpan{text: text, x: x, y: y, action: action})
		return x + ansi.StringWidth(text)
	}
	modelLines := strings.Split(vm.ModelInfo, "\n")
	modelName, provider := "", ""
	if len(modelLines) > 0 {
		modelName = modelLines[0]
	}
	if len(modelLines) > 1 {
		provider = modelLines[1]
	}
	// Compact mode spends its two rows on distinct model and provider lines.
	// Workspace/status extras yield before these controls or canonical identity.
	modelName = ansi.Truncate(collapsedSingleLine(modelName), width/2, "…")
	titleBudget := width
	if modelName != "" {
		titleBudget = max(0, width-ansi.StringWidth(modelName)-2)
	}
	appendSpan(vm.TitleWithStar, 0, 0, titleBudget, ClickTitle)
	appendSpan(modelName, width-ansi.StringWidth(modelName), 0, ansi.StringWidth(modelName), ClickModel)

	pill := ansi.Truncate(collapsedSingleLine(vm.Yolo), width, "…")
	usageBudget := width
	if pill != "" {
		usageBudget = max(0, width-ansi.StringWidth(pill)-1)
	}
	usage := ansi.Truncate(collapsedSingleLine(vm.UsageSummary), usageBudget, "…")
	rightWidth := ansi.StringWidth(usage)
	if pill != "" {
		rightWidth += ansi.StringWidth(pill)
		if usage != "" {
			rightWidth++
		}
	}
	leftBudget := width
	if rightWidth > 0 {
		leftBudget = max(0, width-rightWidth-2)
	}
	x := appendSpan(vm.AgentIdentity, 0, 1, leftBudget, ClickNone)
	if x > 0 {
		x += 2
	}
	modelX := x
	for _, span := range modelFooterSpans(provider, vm.Thinking, vm.CanCycleThinking, max(0, leftBudget-modelX)) {
		x = appendSpan(span.text, modelX+span.x, 1, leftBudget-modelX-span.x, span.action)
	}
	for _, part := range []struct {
		text   string
		action ClickResult
	}{
		{vm.WorkingDir, ClickWorkingDir},
		{vm.Branch, ClickNone},
	} {
		if part.text == "" || x >= leftBudget {
			continue
		}
		if x > 0 {
			x += 2
		}
		x = appendSpan(part.text, x, 1, leftBudget-x, part.action)
	}
	appendSpan(usage, width-rightWidth, 1, ansi.StringWidth(usage), ClickUsage)
	appendSpan(pill, width-ansi.StringWidth(pill), 1, ansi.StringWidth(pill), ClickNone)
	return spans
}

func (vm CollapsedViewModel) LineCount() int {
	lines := 1
	for _, span := range vm.spans() {
		lines = max(lines, span.y+1)
	}
	return lines
}

func (CollapsedViewModel) titleSectionLines() int { return 1 }

// RenderCollapsedView clips each semantic span before composing either row.
func RenderCollapsedView(vm CollapsedViewModel) string {
	lines := make([]string, vm.LineCount())
	for _, span := range vm.spans() {
		lines[span.y] += strings.Repeat(" ", max(0, span.x-ansi.StringWidth(lines[span.y]))) + span.text
	}
	return strings.Join(lines, "\n")
}
