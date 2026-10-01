package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

type subagentRowGeometry struct {
	start, end, width int
	chevron           int
}

type subagentHover struct {
	value, target float64
}

// Roots have no implied super-root; only descendants own tree connectors.
func subagentGuides(row subagentview.Row) string {
	if row.Depth == 0 {
		return ""
	}
	return strings.ReplaceAll(ansi.TruncateLeft(row.Guides, 2, ""), "─", " ")
}

func (d *subagentsDialog) renderRows(width int) ([]string, []subagentRowGeometry, []int) {
	var lines []string
	var geometry []subagentRowGeometry
	var owners []int
	for i, row := range d.rows {
		if i > 0 && row.Depth == 0 && (row.Branch || d.rows[i-1].Depth > 0 || d.rows[i-1].Branch) {
			lines, owners = append(lines, ""), append(owners, -1)
		}
		g := subagentRowGeometry{start: len(lines), width: width, chevron: -1}
		lead := "  "
		if i == d.selected {
			lead = styles.HighlightWhiteStyle.Render("› ")
		}
		guides := subagentGuides(row)
		guides = ansi.TruncateLeft(guides, max(0, ansi.StringWidth(guides)-max(0, min(width/4, width-18))), "")
		prefix := lead + styles.MutedStyle.Render(guides)
		room := max(1, width-ansi.StringWidth(prefix))
		state := cleanDetail(string(row.Node.State))
		if row.Node.State == subagent.NodeRunning && row.Node.WaitingOn == "" {
			frame := d.spinnerFrame
			if frame == "" {
				frame = animation.TabBusy.FrameAt(0)
			}
			state = frame + " " + state
		}
		if row.Node.WaitingOn != "" {
			state += " · waiting: " + cleanDetail(row.Node.WaitingOn)
		}
		if row.Node.NeedsAttention {
			state += " · ⚠ attention"
		}
		fullState := state
		stateWidth := min(ansi.StringWidth(state), max(0, room-20))
		if stateWidth < 4 || stateWidth < ansi.StringWidth(state) {
			stateWidth = 0
		}
		identityRoom := room
		if stateWidth > 0 {
			identityRoom -= stateWidth + 2
		}
		controlWidth := 0
		if row.Branch && identityRoom >= 4 {
			controlWidth = 2
		}
		name := ansi.Truncate(cleanDetail(row.Node.DisplayName()), max(1, identityRoom-controlWidth), "…")
		hover := d.hover[row.Node.ID].value
		identity := styles.HoverText(styles.AgentIdentityStyle(row.Node.Agent, false).Render(name), hover, styles.TextPrimary)
		primary := prefix + identity
		if controlWidth > 0 {
			g.chevron = ansi.StringWidth(primary) + 1
			chevron := "⌄"
			if d.collapsed[row.Node.ID] {
				chevron = "›"
			}
			primary += " " + styles.HoverText(styles.MutedStyle.Render(chevron), hover, styles.TextPrimary)
		}
		if stateWidth > 0 {
			state = ansi.Truncate(state, stateWidth, "…")
			primary += strings.Repeat(" ", max(2, width-ansi.StringWidth(primary)-ansi.StringWidth(state))) + styles.MutedStyle.Render(state)
		}
		lines = append(lines, ansi.Truncate(primary, width, ""))
		owners = append(owners, i)
		if g.chevron >= width {
			g.chevron = -1
		}
		// Continue ancestor rails on title lines, but never extend the leaf connector.
		continuation := guides
		if strings.HasSuffix(continuation, "├ ") {
			continuation = strings.TrimSuffix(continuation, "├ ") + "│ "
		} else if strings.HasSuffix(continuation, "└ ") {
			continuation = strings.TrimSuffix(continuation, "└ ") + "  "
		}
		detailPrefix := "  " + continuation
		detailRoom := width - ansi.StringWidth(detailPrefix)
		if ansi.StringWidth(fullState) > stateWidth && detailRoom >= 8 {
			for _, line := range strings.Split(ansi.Wrap(fullState, detailRoom, ""), "\n") {
				lines = append(lines, styles.MutedStyle.Render(detailPrefix+line))
				owners = append(owners, i)
			}
		}
		if title := cleanDetail(d.titles[row.Node.SessionID]); title != "" && detailRoom >= 8 {
			wrapped := strings.Split(ansi.Wrap(title, detailRoom, ""), "\n")
			for j, line := range wrapped {
				if j == 1 {
					line = ansi.Truncate(strings.Join(wrapped[j:], " "), detailRoom, "…")
				}
				text := styles.SecondaryStyle.Render(line)
				lines = append(lines, styles.MutedStyle.Render(detailPrefix)+styles.HoverText(text, hover, styles.TextPrimary))
				owners = append(owners, i)
				if j == 1 {
					break
				}
			}
		}
		g.end = len(lines)
		geometry = append(geometry, g)
	}
	return lines, geometry, owners
}

func (d *subagentsDialog) rowForLine(line int) int {
	if line < 0 || line >= len(d.lineRows) {
		return -1
	}
	return d.lineRows[line]
}

func (d *subagentsDialog) rowAt(x, y int) int {
	if !d.pointerInBody(x, y) {
		return -1
	}
	index := d.rowForLine(d.scrollview.ScrollOffset() + y - d.bodyY)
	if index < 0 || index >= len(d.prepared) || x >= d.bodyX+d.prepared[index].width {
		return -1
	}
	return index
}

func (d *subagentsDialog) pointerInBody(x, y int) bool {
	return d.Height() >= 5 && x >= d.bodyX && x < min(d.Width()-1, d.bodyX+d.bodyWidth-d.scrollview.ReservedCols()) && y >= d.bodyY && y < min(d.Height()-2, d.bodyY+d.bodyHeight)
}

func (d *subagentsDialog) selectedLine() int {
	if d.selected >= 0 && d.selected < len(d.prepared) {
		return d.prepared[d.selected].start
	}
	return 0
}

func (d *subagentsDialog) ensureSelectedVisible() {
	if d.selected >= 0 && d.selected < len(d.prepared) {
		g := d.prepared[d.selected]
		d.scrollview.EnsureRangeVisible(g.start, g.end-1)
	}
}

func (d *subagentsDialog) page(direction int) {
	line := d.selectedLine() + direction*d.scrollview.VisibleHeight()
	line = max(0, min(line, len(d.lineRows)-1))
	for line >= 0 && line < len(d.lineRows) {
		if index := d.rowForLine(line); index >= 0 {
			d.selected = index
			d.ensureSelectedVisible()
			return
		}
		line += direction
	}
}

func (d *subagentsDialog) BindAnimationRuntime(runtime *animation.Runtime) {
	d.hoverAnimation.SetRuntime(runtime)
	d.spinnerAnimation.SetRuntime(runtime)
	d.runtime = runtime
	d.animationBound = true
}

func (d *subagentsDialog) syncHover() tea.Cmd {
	id := subagent.NodeID("")
	if d.pointerKnown {
		if i := d.rowAt(d.pointerX, d.pointerY); i >= 0 {
			id = d.rows[i].Node.ID
		}
	}
	if id == d.hovered {
		return nil
	}
	d.hovered = id
	if d.hover == nil {
		d.hover = make(map[subagent.NodeID]subagentHover)
	}
	if id != "" {
		if _, ok := d.hover[id]; !ok {
			d.hover[id] = subagentHover{}
		}
	}
	for key, state := range d.hover {
		target := 0.0
		if key == id {
			target = 1
		}
		if state.target != target {
			state.target = target
			if !d.animationBound {
				state.value = target
			}
			d.hover[key] = state
		}
	}
	d.MarkVisualDirty()
	if d.animationBound {
		return d.hoverAnimation.Start()
	}
	return nil
}

func (d *subagentsDialog) tickHover(tick animation.TickMsg) {
	if !d.hoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	running := false
	for id, state := range d.hover {
		state.value = animation.HoverStep(state.value, state.target, after-before)
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(d.hover, id)
		} else {
			d.hover[id] = state
		}
	}
	if !running {
		d.hoverAnimation.Stop()
	}
	d.MarkVisualDirty()
	tick.MarkDirty()
}

func (d *subagentsDialog) syncSpinner() {
	if !d.animationBound {
		return
	}
	if d.Height() < 5 || d.Width() < 8 {
		d.spinnerAnimation.Stop()
		return
	}
	start, end := d.scrollview.ScrollOffset(), d.scrollview.ScrollOffset()+d.scrollview.VisibleHeight()
	for i, row := range d.rows {
		if i < len(d.prepared) && d.prepared[i].end > start && d.prepared[i].start < end && row.Node.State == subagent.NodeRunning && row.Node.WaitingOn == "" {
			d.spinnerAnimation.Start()
			return
		}
	}
	d.spinnerAnimation.Stop()
}

func (d *subagentsDialog) tickSpinner(tick animation.TickMsg) {
	if !d.spinnerAnimation.IsActive() {
		return
	}
	frame := animation.TabBusy.FrameAt(d.runtime.Now())
	if frame != d.spinnerFrame {
		d.spinnerFrame = frame
		d.MarkVisualDirty()
		tick.MarkDirty()
	}
}

func (d *subagentsDialog) StopAnimations() {
	d.hoverAnimation.Stop()
	d.spinnerAnimation.Stop()
	d.hover, d.hovered, d.pointerKnown = nil, "", false
	d.MarkVisualDirty()
}

// The manager calls this when another dialog occludes this one.
func (d *subagentsDialog) ResetCloseHover() {
	d.BaseDialog.ResetCloseHover()
	d.StopAnimations()
}
