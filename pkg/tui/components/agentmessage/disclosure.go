// Package agentmessage shares the inter-agent message surface and disclosure.
package agentmessage

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Disclosure struct {
	motion        animation.Transition
	expanded      bool
	from, visible float64
}

func New(ar *animation.Runtime) Disclosure {
	return Disclosure{motion: ar.Transition()}
}

func (d *Disclosure) Expanded() bool  { return d.expanded }
func (d *Disclosure) NeedsTick() bool { return d.motion.Running() }
func (d *Disclosure) Visible() bool   { return d.visible > 0 }
func (d *Disclosure) Chevron() string {
	if d.expanded {
		return "v"
	}
	return ">"
}

func (d *Disclosure) Toggle() {
	d.from = d.visible
	d.expanded = !d.expanded
	d.motion.Start(140*time.Millisecond, animation.EaseOutCubic)
}

func (d *Disclosure) Tick(tick animation.TickMsg) {
	if !d.motion.Running() {
		return
	}
	d.motion.Tick()
	target := 0.0
	if d.expanded {
		target = 1
	}
	d.visible = d.from + (target-d.from)*d.motion.Value()
	tick.MarkDirty()
}

// Settle releases the lease when hidden, removed or resized.
func (d *Disclosure) Settle() {
	d.motion.Cancel()
	d.visible = 0
	if d.expanded {
		d.visible = 1
	}
}

func (d *Disclosure) Render(header, body string) string {
	if d.visible <= 0 || body == "" {
		return header
	}
	lines := strings.Split(body, "\n")
	rows := min(len(lines), int(float64(len(lines))*d.visible+0.5))
	if rows == 0 {
		return header
	}
	return header + "\n" + strings.Join(lines[:rows], "\n")
}

func ToggleAt(header string, line, col int) bool {
	lines := strings.Split(header, "\n")
	if line != len(lines)-1 {
		return false
	}
	return col == ansi.StringWidth(strings.TrimRight(ansi.Strip(lines[line]), " "))-1
}

func Body(content string, ref lifecycle.InputReference, width int, selected bool, actions string) string {
	style := styles.UserMessageStyle.Bold(false)
	if selected {
		style = styles.SelectedUserMessageStyle.Bold(false)
	}
	content = strings.ReplaceAll(content, "\t", "    ")
	rendered := style.PaddingTop(0).Width(width).Render(actions + "\n" + strings.TrimRight(content, "\n\r\t "))
	return agentidentity.Border(rendered, ref, width, style)
}

func InnerWidth(width int) int {
	return max(0, width-styles.UserMessageStyle.GetHorizontalFrameSize())
}

// PreserveExpansion carries view-local state across tool status replacements.
func PreserveExpansion(previous, next any) {
	old, ok := previous.(interface{ IsExpanded() bool })
	if !ok {
		return
	}
	if view, ok := next.(interface{ SetExpanded(expanded bool) }); ok {
		view.SetExpanded(old.IsExpanded())
	}
}
