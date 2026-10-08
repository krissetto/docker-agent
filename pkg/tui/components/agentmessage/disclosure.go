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

const toggleLink = "docker-agent:disclosure"

func (d *Disclosure) Header(prefix string, ref lifecycle.InputReference, status string, width int) string {
	return CompactHeader(prefix, ref, status, d.Chevron(), width)
}

func CompactHeader(prefix string, ref lifecycle.InputReference, status, chevron string, width int) string {
	control := ansi.SetHyperlink(toggleLink) + styles.MutedStyle.Render(chevron) + ansi.ResetHyperlink()
	return agentidentity.Wrap(prefix, ref, styles.MutedStyle.Render(status+" ")+control, width)
}

func Header(prefix string, ref lifecycle.InputReference, status, chevron string, width int) string {
	return CompactHeader(prefix, ref, status, chevron, width)
}

func HeaderAt(header string, line, col, width int) bool {
	return line >= 0 && line <= strings.Count(header, "\n") && col >= 0 && col < width
}

func ToggleAt(header string, line, col int) bool {
	lines := strings.Split(header, "\n")
	if line < 0 || line >= len(lines) {
		return false
	}
	prefix, _, found := strings.Cut(lines[line], ansi.SetHyperlink(toggleLink))
	return found && col == ansi.StringWidth(prefix)
}

func Body(content string, width int, selected bool, actions string) string {
	style := styles.UserMessageStyle.Bold(false)
	if selected {
		style = styles.SelectedUserMessageStyle.Bold(false)
	}
	content = strings.ReplaceAll(content, "\t", "    ")
	return style.PaddingTop(0).Width(width).Render(actions + "\n" + strings.TrimRight(content, "\n\r\t "))
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
