package messagebar

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Control is persistent presentation, independent from transient notice ownership.
type Control struct {
	ID, Label string
	Active    bool
	Command   tea.Cmd
}
type controlBounds struct {
	index, start, end int
	label             string
}

func (m *Model) SetControls(controls []Control) {
	equal := len(controls) == len(m.controls)
	if equal {
		for i, c := range controls {
			old := m.controls[i]
			if c.ID != old.ID || c.Label != old.Label || c.Active != old.Active {
				equal = false
				break
			}
		}
	}
	m.controls = slices.Clone(controls)
	if equal {
		return
	}
	for i := range m.controls {
		m.controls[i].Label = singleLine(m.controls[i].Label)
	}
	m.layout()
	m.invalidate()
	m.prepareView()
}
func (m *Model) layoutControls(budget int) {
	if budget < 3 {
		return
	}
	order := make([]int, 0, len(m.controls))
	for i, c := range m.controls {
		if c.Active {
			order = append(order, i)
		}
	}
	for i, c := range m.controls {
		if !c.Active {
			order = append(order, i)
		}
	}
	for _, i := range order {
		c := m.controls[i]
		remaining := budget - m.controlWidth
		if remaining < 3 {
			break
		}
		label := ansi.Truncate(c.Label, remaining-2, "…")
		width := ansi.StringWidth(label) + 2
		if label == "" || c.Command == nil {
			continue
		}
		m.controlBounds = append(m.controlBounds, controlBounds{index: i, start: m.controlWidth, end: m.controlWidth + width, label: label})
		m.controlWidth += width
	}
	if m.controlWidth > 0 {
		m.controlWidth++
	}
}
func (m *Model) ControlClick(x, y int) tea.Cmd {
	if y != 0 || m.height == 0 {
		return nil
	}
	for _, b := range m.controlBounds {
		if x >= b.start && x < b.end {
			return m.controls[b.index].Command
		}
	}
	return nil
}
func (m *Model) renderControls() string {
	var out strings.Builder
	for _, b := range m.controlBounds {
		style := styles.MutedStyle
		if m.controls[b.index].Active {
			style = styles.HighlightWhiteStyle
		}
		out.WriteString(style.Render(" " + b.label + " "))
	}
	if m.controlWidth > 0 {
		out.WriteByte(' ')
	}
	return out.String()
}

// SetFallback supplies persistent activity truth without acquiring notice ownership.
func (m *Model) SetFallback(text string) {
	text = singleLine(text)
	if m.fallback == text {
		return
	}
	m.fallback = text
	m.layout()
	m.invalidate()
	m.prepareView()
}
