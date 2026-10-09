// Package agentidentity renders the small shared identity label and its hit span.
package agentidentity

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const Link = "docker-agent:identity"

var nameLink = ansi.SetHyperlink(Link, "id=docker-agent-identity-name")

func Label(ref lifecycle.InputReference, width int) string {
	text := ansi.Truncate(ref.Label(), max(width, 0), "…")
	if text == "" {
		return ""
	}
	if ref.Kind == lifecycle.InputReferenceUnknown {
		return styles.MutedStyle.Render(text)
	}
	nameWidth := min(ansi.StringWidth(ref.Name), ansi.StringWidth(text))
	name := ansi.Cut(text, 0, nameWidth)
	suffix := ansi.Cut(text, nameWidth, ansi.StringWidth(text))
	label := nameLink + styles.AgentIdentityStyle(ref.Agent, false).Render(name) + ansi.ResetHyperlink()
	if suffix != "" {
		label += styles.MutedStyle.Render(suffix)
	}
	return label
}

// Hover keeps compatibility with callers that have a boolean hover state.
func Hover(line string, startCol, endCol int, ref lifecycle.InputReference) string {
	return HoverProgress(line, startCol, endCol, ref, 1)
}

// HoverProgress highlights only the linked name run, never its neutral ID suffix.
func HoverProgress(line string, startCol, endCol int, ref lifecycle.InputReference, progress float64) string {
	if progress <= 0 || ref.Kind == lifecycle.InputReferenceUnknown {
		return line
	}
	startCol = max(0, startCol)
	endCol = min(ansi.StringWidth(line), endCol)
	if endCol <= startCol {
		return line
	}
	// Work on byte ranges, not ANSI cuts: Cut replays unrelated OSC/style
	// sequences and can multiply them when name and suffix share a row.
	var out strings.Builder
	cursor := 0
	for cursor < len(line) {
		offset := strings.Index(line[cursor:], nameLink)
		if offset < 0 {
			break
		}
		nameStart := cursor + offset + len(nameLink)
		nameEnd := len(line)
		if end := strings.Index(line[nameStart:], ansi.ResetHyperlink()); end >= 0 {
			nameEnd = nameStart + end
		}
		from := ansi.StringWidth(line[:nameStart])
		to := from + ansi.StringWidth(line[nameStart:nameEnd])
		out.WriteString(line[cursor:nameStart])
		if from >= startCol && to <= endCol && to > from {
			out.WriteString(styles.HoverText(line[nameStart:nameEnd], progress, nil))
		} else {
			out.WriteString(line[nameStart:nameEnd])
		}
		cursor = nameEnd
	}
	out.WriteString(line[cursor:])
	return out.String()
}

// Border embeds the identity in the top border without changing the body rows.
// Callers own body rendering and any shell-integration markers around the result.
func Border(rendered string, ref lifecycle.InputReference, width int, messageStyle lipgloss.Style) string {
	lines := strings.Split(rendered, "\n")
	if width < 1 {
		return rendered
	}
	border := messageStyle.GetBorderStyle()
	if border.Top == "" {
		border = lipgloss.NormalBorder()
	}
	surface := lipgloss.NewStyle().Foreground(messageStyle.GetForeground()).Background(messageStyle.GetBackground())
	ruleStyle := surface.Foreground(messageStyle.GetBorderLeftForeground())
	if messageStyle.GetBorderLeftSize() == 0 {
		ruleStyle = surface.Foreground(styles.BorderPrimary)
	}
	left := ""
	if messageStyle.GetBorderLeftSize() > 0 {
		left = ruleStyle.Render(border.TopLeft)
	}
	innerWidth := max(0, width-ansi.StringWidth(left))
	lead := ansi.Truncate(border.Top+" ", innerWidth, "")
	label := Label(ref, max(0, innerWidth-ansi.StringWidth(lead)-2))
	tailWidth := max(0, innerWidth-ansi.StringWidth(lead)-ansi.StringWidth(label))
	tail := strings.Repeat(border.Top, tailWidth)
	if label != "" && tailWidth > 0 {
		tail = " " + strings.Repeat(border.Top, tailWidth-1)
	}
	lines[0] = left + styles.RenderComposite(surface, ruleStyle.Render(lead)+label+ruleStyle.Render(tail))
	return strings.Join(lines, "\n")
}

// Wrap keeps each wrapped name fragment independently hit-testable.
func Wrap(prefix string, ref lifecycle.InputReference, suffix string, width int) string {
	text := ansi.Wrap(prefix+Label(ref, ansi.StringWidth(ref.Label()))+suffix, max(width, 1), "")
	reset := ansi.ResetHyperlink()
	lines := strings.Split(text, "\n")
	active := false
	for i, line := range lines {
		if active {
			line = nameLink + ansi.Style{}.ForegroundColor(styles.AgentIdentityStyle(ref.Agent, false).GetForeground()).String() + line
		}
		active = strings.LastIndex(line, nameLink) > strings.LastIndex(line, reset)
		if active {
			line += reset
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
