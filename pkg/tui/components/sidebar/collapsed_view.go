package sidebar

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// CollapsedViewModel holds the content of the compact horizontal sidebar.
// Rendering and hit testing share the same bounded two-row layout.
type CollapsedViewModel struct {
	TitleWithStar    string
	WorkingDir       string
	Branch           string
	Yolo             string
	AgentIdentity    string
	ModelInfo        string
	Thinking         string
	CanCycleThinking bool
	UsageSummary     string
	// PlansSummary is the collapsed Plans link. It shares the info row, so the
	// bounded two-row band keeps its geometry.
	PlansSummary string

	ContentWidth int
}

var collapsedWhitespace = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")

type collapsedSpan struct {
	text   string
	x, y   int
	action ClickResult
}

func collapsedSingleLine(text string) string {
	return collapsedWhitespace.Replace(text)
}

// spans reserves the right-hand usage summary before fitting optional metadata.
// Long and multiline titles or model descriptions can never grow the band.
func (vm CollapsedViewModel) spans() []collapsedSpan {
	width := max(0, vm.ContentWidth)
	var spans []collapsedSpan
	appendSpan := func(text string, x, y, budget int, action ClickResult) int {
		text = clipCollapsed(text, budget)
		if ansi.StringWidth(text) == 0 {
			return x
		}
		spans = append(spans, collapsedSpan{text: text, x: x, y: y, action: action})
		return x + ansi.StringWidth(text)
	}
	modelName, provider, _ := strings.Cut(vm.ModelInfo, "\n")
	// Compact mode spends its two rows on distinct model and provider lines.
	// Workspace/status extras yield before these controls or canonical identity.
	modelName = clipCollapsed(modelName, width/2)
	titleBudget := collapsedTitleWidth(width, modelName)
	appendSpan(vm.TitleWithStar, 0, 0, titleBudget, ClickTitle)
	appendSpan(modelName, width-ansi.StringWidth(modelName), 0, ansi.StringWidth(modelName), ClickModel)

	pill := clipCollapsed(vm.Yolo, width)
	usageBudget := width
	if pill != "" {
		usageBudget = max(0, width-ansi.StringWidth(pill)-1)
	}
	usage := clipCollapsed(vm.UsageSummary, usageBudget)
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
	appendLeft := func(text string, action ClickResult) {
		if text == "" {
			return
		}
		start := x
		if x > 0 {
			start += minGap
		}
		if start < leftBudget {
			x = appendSpan(text, start, 1, leftBudget-start, action)
		}
	}
	appendLeft(provider, ClickModel)
	if vm.Thinking != "" && x+1+ansi.StringWidth(vm.Thinking) <= leftBudget {
		action := ClickNone
		if vm.CanCycleThinking {
			action = ClickThinkingLevel
		}
		x = appendSpan(vm.Thinking, x+1, 1, leftBudget-x-1, action)
	}
	appendLeft(vm.WorkingDir, ClickWorkingDir)
	appendLeft(vm.Branch, ClickNone)
	appendLeft(vm.PlansSummary, ClickPlanBrowser)
	appendSpan(usage, width-rightWidth, 1, ansi.StringWidth(usage), ClickUsage)
	appendSpan(pill, width-ansi.StringWidth(pill), 1, ansi.StringWidth(pill), ClickNone)
	return spans
}

func clipCollapsed(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	return ansi.Truncate(collapsedSingleLine(text), budget, "…")
}

func collapsedTitleWidth(width int, model string) int {
	if model == "" {
		return width
	}
	return max(0, width-ansi.StringWidth(model)-minGap)
}

func collapsedLineCount(spans []collapsedSpan) int {
	lines := 1
	for _, span := range spans {
		lines = max(lines, span.y+1)
	}
	return lines
}

func (vm CollapsedViewModel) LineCount() int { return collapsedLineCount(vm.spans()) }

// RenderCollapsedView clips each semantic span before composing either row.
func RenderCollapsedView(vm CollapsedViewModel) string {
	return renderCollapsedSpans(vm.spans())
}

func renderCollapsedSpans(spans []collapsedSpan) string {
	lines := make([]string, collapsedLineCount(spans))
	for _, span := range spans {
		lines[span.y] += strings.Repeat(" ", max(0, span.x-ansi.StringWidth(lines[span.y]))) + span.text
	}
	return strings.Join(lines, "\n")
}
