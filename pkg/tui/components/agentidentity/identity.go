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
		label += ansi.SetHyperlink(Link) + styles.MutedStyle.Render(suffix) + ansi.ResetHyperlink()
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
	segment := ansi.Cut(line, startCol, endCol)
	nameStart := strings.Index(segment, nameLink)
	if nameStart < 0 {
		return line
	}
	nameStart += len(nameLink)
	nameEnd := len(segment)
	if end := strings.Index(segment[nameStart:], ansi.ResetHyperlink()); end >= 0 {
		nameEnd = nameStart + end
	}
	segment = segment[:nameStart] + styles.HoverText(segment[nameStart:nameEnd], progress, nil) + segment[nameEnd:]
	return ansi.Cut(line, 0, startCol) + segment + ansi.Cut(line, endCol, ansi.StringWidth(line))
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

// Wrap keeps each wrapped identity fragment independently hit-testable.
func Wrap(prefix string, ref lifecycle.InputReference, suffix string, width int) string {
	text := ansi.Wrap(prefix+Label(ref, ansi.StringWidth(ref.Label()))+suffix, max(width, 1), "")
	open, reset := ansi.SetHyperlink(Link), ansi.ResetHyperlink()
	lines := strings.Split(text, "\n")
	active := ""
	for i, line := range lines {
		if active != "" {
			line = active + line
		}
		lastName, lastOpen, lastClose := strings.LastIndex(line, nameLink), strings.LastIndex(line, open), strings.LastIndex(line, reset)
		switch {
		case lastName > lastClose && lastName > lastOpen:
			active = nameLink
		case lastOpen > lastClose:
			active = open
		default:
			active = ""
		}
		if active != "" {
			line += reset
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
