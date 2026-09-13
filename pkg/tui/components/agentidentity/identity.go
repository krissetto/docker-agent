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
	return ansi.SetHyperlink(Link) + styles.AgentIdentityStyle(ref.Agent, false).Render(name) + styles.MutedStyle.Render(suffix) + ansi.ResetHyperlink()
}

// Hover changes only the name's accent, preserving the suffix, link and surface.
func Hover(line string, startCol, endCol int, ref lifecycle.InputReference) string {
	sequence := func(hovered bool) string {
		styled := styles.AgentIdentityStyle(ref.Agent, hovered).Render("x")
		return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(styled, "\x1b[0m"), "\x1b[m"), "x")
	}
	normal, bright := sequence(false), sequence(true)
	if normal == "" {
		return line
	}
	segment := ansi.Cut(line, startCol, endCol)
	return ansi.Cut(line, 0, startCol) + strings.ReplaceAll(segment, normal, bright) + ansi.Cut(line, endCol, ansi.StringWidth(line))
}

// Border insets an identity in the ordinary USER padding row without a top rule.
func Border(rendered string, ref lifecycle.InputReference, width int, messageStyle lipgloss.Style) string {
	lines := strings.Split(rendered, "\n")
	if len(lines) == 0 || width < 1 {
		return rendered
	}
	leftWidth := min(width, messageStyle.GetBorderLeftSize())
	left := ansi.Cut(lines[0], 0, leftWidth)
	innerWidth := max(0, width-leftWidth)
	inset := min(messageStyle.GetPaddingLeft(), innerWidth)
	label := Label(ref, max(0, innerWidth-inset-messageStyle.GetPaddingRight()))
	surface := lipgloss.NewStyle().Foreground(messageStyle.GetForeground()).Background(messageStyle.GetBackground())
	lines[0] = left + styles.RenderComposite(surface, strings.Repeat(" ", inset)+label+strings.Repeat(" ", max(0, innerWidth-inset-ansi.StringWidth(label))))
	return strings.Join(lines, "\n")
}

// Wrap keeps each wrapped identity fragment independently hit-testable.
func Wrap(prefix string, ref lifecycle.InputReference, suffix string, width int) string {
	text := ansi.Wrap(prefix+Label(ref, ansi.StringWidth(ref.Label()))+suffix, max(width, 1), "")
	open, reset := ansi.SetHyperlink(Link), ansi.ResetHyperlink()
	lines := strings.Split(text, "\n")
	active := false
	for i, line := range lines {
		if active {
			line = open + line
		}
		lastOpen, lastClose := strings.LastIndex(line, open), strings.LastIndex(line, reset)
		active = lastOpen > lastClose
		if active {
			line += reset
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
