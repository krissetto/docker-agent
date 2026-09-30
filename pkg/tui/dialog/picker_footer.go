package dialog

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// RenderPickerFooter gives only the primary action button chrome. Auxiliary
// shortcuts share the same measured action registry without masquerading as buttons.
func (b *BaseDialog) RenderPickerFooter(width int, actions ...Action) string {
	width = max(1, width)
	b.actions = append(b.actions[:0], actions...)
	b.actionRows = nil
	b.actionLines = make([]int, len(actions))
	b.pickerFooter = true
	selected := b.selectedAction(actions)
	primary := -1
	for i, a := range actions {
		if a.Primary || (a.Key.Code == tea.KeyEnter && a.Key.Mod == 0) {
			primary = i
			break
		}
	}
	var lines []string
	var line string
	var hits []dialogActionHit
	flush := func() {
		if line == "" {
			return
		}
		b.actionRows = append(b.actionRows, dialogActionRow{text: ansi.Strip(line), hits: hits})
		lines = append(lines, line)
		line = ""
		hits = nil
	}
	for i, a := range actions {
		if i == primary {
			continue
		}
		text := a.Key.Keystroke() + " " + strings.TrimSpace(a.Label)
		style := styles.MutedStyle
		if i == selected && !a.Disabled {
			style = style.Foreground(styles.TextPrimary).Underline(true)
		}
		for j, part := range strings.Split(ansi.Hardwrap(text, width, true), "\n") {
			if line != "" && lipgloss.Width(line)+2+ansi.StringWidth(part) > width {
				flush()
			}
			if line != "" {
				line += "  "
			}
			if j == 0 {
				b.actionLines[i] = len(lines)
			}
			x := lipgloss.Width(line)
			paint := style.Render(part)
			if b.footerHoverKey == a.Key.Keystroke() && !a.Disabled {
				paint = styles.HoverText(paint, b.footerHoverAlpha, styles.TextPrimary)
			}
			line += paint
			if !a.Disabled {
				hits = append(hits, dialogActionHit{x: x, width: ansi.StringWidth(part), key: a.Key})
			}
			if j > 0 {
				flush()
			}
		}
	}
	if primary >= 0 {
		a := actions[primary]
		label := a.Label
		if primary == selected {
			label += " ↵"
		} else {
			label += "  "
		}
		style := styles.NoStyle.Padding(0, 1).Bold(true).Foreground(styles.TextPrimary).Background(styles.BackgroundAlt)
		if primary == selected {
			style = style.Foreground(styles.SelectedFg).Background(styles.Selected)
		}
		if a.Disabled {
			style = style.Foreground(styles.TextMuted).Bold(false)
		}
		pill := style.Render(ansi.Truncate(label, max(1, width-2), ""))
		pillWidth := lipgloss.Width(pill)
		if line != "" && lipgloss.Width(line)+2+pillWidth > width {
			flush()
		}
		x := max(0, width-pillWidth)
		line += strings.Repeat(" ", max(0, x-lipgloss.Width(line))) + pill
		b.actionLines[primary] = len(lines)
		if !a.Disabled {
			hits = append(hits, dialogActionHit{x: x, width: pillWidth, key: a.Key})
		}
	}
	flush()
	return strings.Join(lines, "\n")
}

// PickerFooterHelp appends noninteractive navigation without inventing action targets.
func (b *BaseDialog) PickerFooterHelp(footer, help string, width int) string {
	text := styles.MutedStyle.Render(ansi.Wrap(help, max(1, width), ""))
	for _, line := range strings.Split(text, "\n") {
		b.actionRows = append(b.actionRows, dialogActionRow{text: ansi.Strip(line)})
	}
	if footer == "" {
		return text
	}
	return footer + "\n" + text
}

func (b *BaseDialog) UpdateFooterHover(msg tea.Msg, runtime *animation.Runtime, dl DialogLayout) tea.Cmd {
	if !b.pickerFooter {
		return nil
	}
	switch msg := msg.(type) {
	case tea.MouseMotionMsg:
		key := ""
		if action, ok := b.ActionKeyAt(msg.X, msg.Y, dl); ok && action.Code != tea.KeyEnter {
			key = action.Keystroke()
		}
		if key == b.footerHoverKey {
			return nil
		}
		b.footerHoverKey = key
		b.footerHoverAlpha = 0
		b.footerHover.Cancel()
		b.MarkVisualDirty()
		if key != "" {
			b.footerHover = runtime.Transition()
			return b.footerHover.Start(100*time.Millisecond, animation.Linear)
		}
	case animation.TickMsg:
		if b.footerHover.Running() {
			b.footerHover.Tick()
			b.footerHoverAlpha = b.footerHover.Value()
			b.MarkVisualDirty()
		}
	}
	return nil
}

func (b *BaseDialog) StopFooterHover() {
	b.footerHover.Cancel()
	b.footerHoverKey = ""
	b.footerHoverAlpha = 0
	b.MarkVisualDirty()
}
