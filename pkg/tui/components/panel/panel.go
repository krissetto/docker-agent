// Package panel lays out independent workspace status elements in the footer.
package panel

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Element is a plain-text status label with a stable action identity.
type Element struct {
	ID    messages.PanelElement
	Label string
}

// Zone is the half-open terminal-column range of a visible element.
type Zone struct {
	ID         messages.PanelElement
	Start, End int
}

// Frame contains the composed row and only its visible action targets.
type Frame struct {
	Content string
	Zones   []Zone
}

// Render gives existing notices and action pills priority and uses only their
// trailing free cells. Zone coordinates are terminal columns, never byte offsets.
func Render(notice string, width int, elements []Element, focused messages.PanelElement) Frame {
	occupied := ansi.StringWidth(strings.TrimRight(ansi.Strip(notice), " "))
	gap := 0
	if occupied > 0 {
		gap = 1
	}
	budget := max(0, width-occupied-gap)
	var labels []string
	var zones []Zone
	used := 0
	for _, element := range elements {
		label := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, ansi.Strip(element.Label))
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		separator := 0
		if len(labels) > 0 {
			separator = 3
		}
		available := budget - used - separator
		if available < 3 {
			break
		}
		label = ansi.Truncate(label, available, "…")
		n := ansi.StringWidth(label)
		if n == 0 {
			break
		}
		if separator > 0 {
			labels = append(labels, styles.MutedStyle.Render(" · "))
			used += separator
		}
		style := styles.MutedStyle
		if element.ID == focused {
			style = style.Underline(true)
		}
		labels = append(labels, style.Render(label))
		zones = append(zones, Zone{element.ID, used, used + n})
		used += n
	}
	if used == 0 {
		return Frame{Content: notice}
	}
	start := width - used
	for i := range zones {
		zones[i].Start += start
		zones[i].End += start
	}
	return Frame{Content: ansi.Cut(notice, 0, occupied) + strings.Repeat(" ", max(0, start-occupied)) + strings.Join(labels, ""), Zones: zones}
}

// Hit identifies an element at a terminal column; separators are not targets.
func (f Frame) Hit(x int) (messages.PanelElement, bool) {
	for _, zone := range f.Zones {
		if x >= zone.Start && x < zone.End {
			return zone.ID, true
		}
	}
	return "", false
}
