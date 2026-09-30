package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// RenderChoices renders mutually exclusive decisions as readable vertical rows,
// rather than an action-button wall. It shares the dialog's keyboard selection,
// hit testing and bounded footer viewport with RenderActions. Clicking a row or
// pressing its mnemonic activates it; arrows only move the Enter selection.
func (b *BaseDialog) RenderChoices(contentWidth int, choices ...Action) string {
	b.actionRows = nil
	b.actions = append(b.actions[:0], choices...)
	b.actionLines = make([]int, len(choices))
	width := max(1, contentWidth)
	indent := min(2, width-1)
	selected := b.selectedAction(choices)
	var rendered []string
	for index, choice := range choices {
		if index > 0 && (choice.Description != "" || choices[index-1].Description != "") {
			b.actionRows = append(b.actionRows, dialogActionRow{})
			rendered = append(rendered, "")
		}
		style := styles.NoStyle.Foreground(styles.TextPrimary)
		prefix := strings.Repeat(" ", indent)
		label := choice.Label
		if index == selected {
			style = style.Foreground(styles.SelectedFg).Background(styles.Selected).Bold(true)
			if indent > 0 {
				prefix = "›" + strings.Repeat(" ", indent-1)
			}
			label += " ↵"
		}
		if !choice.HideShortcut && (choice.Key.Code != 0 && (choice.Key.Code != tea.KeyEnter || choice.Key.Mod != 0)) {
			label += " " + choice.Key.Keystroke()
		}
		if choice.Disabled {
			style = style.Foreground(styles.TextMuted).Bold(false)
		}
		b.actionLines[index] = len(b.actionRows)
		for lineIndex, line := range strings.Split(ansi.Hardwrap(label, max(1, width-indent), true), "\n") {
			leading := strings.Repeat(" ", indent)
			if lineIndex == 0 {
				leading = prefix
			}
			text := leading + line
			text += strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
			row := dialogActionRow{text: ansi.Strip(text)}
			if !choice.Disabled {
				row.hits = []dialogActionHit{{width: width, key: choice.Key}}
			}
			b.actionRows = append(b.actionRows, row)
			rendered = append(rendered, style.Render(text))
		}
		if choice.Description != "" {
			descriptionStyle := styles.NoStyle.Foreground(styles.TextMuted)
			if index == selected {
				descriptionStyle = descriptionStyle.Foreground(styles.SelectedFg).Background(styles.Selected)
			}
			for _, line := range strings.Split(ansi.Hardwrap(choice.Description, max(1, width-indent), true), "\n") {
				text := strings.Repeat(" ", indent) + line
				text += strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
				row := dialogActionRow{text: ansi.Strip(text)}
				if !choice.Disabled {
					row.hits = []dialogActionHit{{width: width, key: choice.Key}}
				}
				b.actionRows = append(b.actionRows, row)
				rendered = append(rendered, descriptionStyle.Render(text))
			}
		}
	}
	return strings.Join(rendered, "\n")
}
