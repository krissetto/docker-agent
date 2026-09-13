// Package agentidentity renders the small shared identity label and its hit span.
package agentidentity

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const Link = "docker-agent:identity"

func Label(ref lifecycle.InputReference, width int) string {
	text := ansi.Truncate(ref.Label(), max(width, 0), "…")
	if ref.Kind == lifecycle.InputReferenceUnknown {
		return styles.MutedStyle.Render(text)
	}
	return ansi.SetHyperlink(Link) + styles.AgentIdentityStyle(ref.Agent, false).Render(text) + ansi.ResetHyperlink()
}

// Border replaces the top padding row with an identity embedded in the border.
func Border(rendered string, ref lifecycle.InputReference, width int) string {
	lines := strings.Split(rendered, "\n")
	if len(lines) == 0 || width < 1 {
		return rendered
	}
	if width < 4 {
		lines[0] = styles.MutedStyle.Render(strings.Repeat("━", width))
		return strings.Join(lines, "\n")
	}
	label := Label(ref, width-4)
	lines[0] = styles.MutedStyle.Render("┏━ ") + label + styles.MutedStyle.Render(" "+strings.Repeat("━", max(width-4-ansi.StringWidth(label), 0)))
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
