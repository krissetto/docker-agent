package dialog

import (
	"maps"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

// SubagentsPolicyMsg updates only the saved policy, never the tree or selection.
type SubagentsPolicyMsg struct{ Enabled bool }

// SubagentsCapabilitiesMsg marks optional controls without affecting browsing.
type SubagentsCapabilitiesMsg struct{ Policy, Stop bool }

type SubagentsRefreshMsg struct {
	Dialog Dialog
	Nodes  []subagent.NodeSnapshot
	Titles map[string]string
}

// subagentsDialog keeps presentation state locally; node IDs alone are routing keys.
type subagentsDialog struct {
	pickerCore
	useSubagents                       bool
	stopTarget                         subagent.NodeID
	stopIdentity                       string
	policyUnavailable, stopUnavailable bool
	nodes                              []subagent.NodeSnapshot
	titles                             map[string]string
	cancel                             func()
	collapsed                          map[subagent.NodeID]bool
	rows                               []subagentview.Row
	prepared                           []subagentRowGeometry
	lineRows                           []int
	hover                              map[subagent.NodeID]subagentHover
	hovered                            subagent.NodeID
	pointerX, pointerY                 int
	pointerKnown                       bool
	hoverAnimation                     animation.Subscription
	animationBound                     bool
	spinnerAnimation                   animation.Subscription
	runtime                            *animation.Runtime
	spinnerFrame                       string
}

func NewSubagentsDialog(nodes []subagent.NodeSnapshot, titles map[string]string, selected ...subagent.NodeID) Dialog {
	d := &subagentsDialog{useSubagents: true, pickerCore: newPickerCore(pickerLayout{WidthPercent: 85, MinWidth: 36, MaxWidth: 120, HeightPercent: 85, MaxHeight: 40, ListOverhead: 6}, ""), nodes: subagentview.Sorted(nodes), titles: maps.Clone(titles), collapsed: map[subagent.NodeID]bool{}}
	d.textInput.Blur()
	id := subagent.NodeID("")
	if len(selected) > 0 {
		id = selected[0]
	}
	d.rebuild(id)
	return d
}
func (d *subagentsDialog) Init() tea.Cmd { return nil }
func (d *subagentsDialog) rebuild(id subagent.NodeID) {
	previous := d.rows
	d.rows = subagentview.Rows(d.nodes, d.collapsed)
	// If a node disappears, prefer its surviving ancestor over an unrelated row.
	for id != "" {
		found := false
		for _, row := range d.rows {
			found = found || row.Node.ID == id
		}
		if found {
			break
		}
		parent := subagent.NodeID("")
		for _, row := range previous {
			if row.Node.ID == id {
				parent = row.Parent
				break
			}
		}
		id = parent
	}
	d.lastClickIndex = -1
	d.selected = min(d.selected, max(0, len(d.rows)-1))
	for i, row := range d.rows {
		if row.Node.ID == id {
			d.selected = i
			break
		}
	}
	d.renderBody(true)
	d.ensureSelectedVisible()
}
func (d *subagentsDialog) selectedID() subagent.NodeID {
	if d.selected >= 0 && d.selected < len(d.rows) {
		return d.rows[d.selected].Node.ID
	}
	return ""
}
func (d *subagentsDialog) attach() tea.Cmd {
	id := d.selectedID()
	if id == "" || !d.claimResponse() {
		return nil
	}
	return tea.Sequence(closeDialogCmd(), core.CmdHandler(messages.OpenSubagentMsg{NodeID: string(id)}))
}
func (d *subagentsDialog) move(delta int) {
	if len(d.rows) == 0 {
		return
	}
	d.selected = (d.selected + delta + len(d.rows)) % len(d.rows)
	d.lastClickIndex = -1
}

func (d *subagentsDialog) branch(expand bool) {
	if len(d.rows) == 0 {
		return
	}
	row := d.rows[d.selected]
	if row.Branch && (expand == d.collapsed[row.Node.ID]) {
		d.collapsed[row.Node.ID] = !expand
		d.rebuild(row.Node.ID)
	} else if expand && row.Branch {
		d.navigate(1, len(d.rows), d.selectedLine)
	} else if !expand && row.Parent != "" {
		d.rebuild(row.Parent)
	}
}
func (d *subagentsDialog) Update(msg tea.Msg) (model layout.Model, cmd tea.Cmd) {
	defer func() {
		if preparesDialogBody(msg) {
			d.renderBody(true)
		}
		if _, key := msg.(tea.KeyPressMsg); key {
			d.ensureSelectedVisible()
		}
		cmd = tea.Batch(cmd, d.syncHover())
		if _, resizing := msg.(tea.WindowSizeMsg); !resizing {
			d.syncSpinner()
		}
	}()
	if _, click := msg.(tea.MouseClickMsg); click && d.stopTarget != "" {
		return d, nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && d.stopTarget != "" {
		target := d.stopTarget
		switch k.String() {
		case "y":
			d.stopTarget = ""
			// A refresh may remove the original target; never retarget confirmation.
			if _, ok := subagentview.Find(d.nodes, target); ok {
				return d, core.CmdHandler(messages.StopSubagentSubtreeMsg{NodeID: string(target)})
			}
		case "n", "esc", "q", "ctrl+c":
			d.stopTarget = ""
		}
		d.renderBody(true)
		return d, nil
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if action, handled := d.HandleActionKey(k); handled {
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
	}
	switch msg := msg.(type) {
	case SubagentsCapabilitiesMsg:
		d.policyUnavailable, d.stopUnavailable = !msg.Policy, !msg.Stop
		if d.stopUnavailable {
			d.stopTarget = ""
		}
		d.renderBody(true)
	case SubagentsPolicyMsg:
		d.useSubagents = msg.Enabled
		d.renderBody(true)
	case animation.TickMsg:
		d.tickHover(msg)
		d.tickSpinner(msg)
	case tea.MouseMotionMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd = d.scrollview.Update(msg)
		return d, cmd
	case tea.MouseWheelMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd = d.scrollview.Update(msg)
		return d, cmd
	case messages.WheelCoalescedMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd = d.scrollview.Update(msg)
		return d, cmd
	case SubagentsRefreshMsg:
		if msg.Dialog != d {
			return d, nil
		}
		id := d.selectedID()
		if msg.Nodes != nil || msg.Titles == nil {
			d.nodes = subagentview.Sorted(msg.Nodes)
		}
		if msg.Titles != nil {
			if d.titles == nil {
				d.titles = make(map[string]string)
			}
			maps.Copy(d.titles, msg.Titles)
		}
		d.rebuild(id)
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		d.lastClickIndex = -1
		switch msg.String() {
		case "esc", "q", "ctrl+c":
			return d, closeDialogCmd()
		case "up", "k":
			d.move(-1)
		case "down", "j":
			d.move(1)
		case "home":
			d.navigate(-d.selected, len(d.rows), d.selectedLine)
		case "end":
			d.navigate(len(d.rows)-1-d.selected, len(d.rows), d.selectedLine)
		case "pgup":
			d.page(-1)
		case "pgdown":
			d.page(1)
		case "left", "h":
			d.branch(false)
		case "right", "l":
			d.branch(true)
		case "space":
			if len(d.rows) > 0 && d.rows[d.selected].Branch {
				d.branch(d.collapsed[d.selectedID()])
			}
		case "s":
			if d.canStop() {
				d.stopTarget = d.selectedID()
				d.stopIdentity = cleanDetail(d.rows[d.selected].Node.DisplayName()) + " (" + cleanDetail(string(d.stopTarget)) + ")"
			}
		case "u":
			if d.policyUnavailable {
				return d, nil
			}
			return d, core.CmdHandler(messages.SetUseSubagentsMsg{Enabled: !d.useSubagents})
		case "enter":
			return d, d.attach()
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		if handled, scrollCmd := d.scrollview.Update(msg); handled {
			return d, scrollCmd
		}
		index := d.rowAt(msg.X, msg.Y)
		if index < 0 {
			d.lastClickIndex = -1
			return d, nil
		}
		twice, _ := d.handleListClick(msg, d.rowForLine)
		geometry := d.prepared[index]
		line := d.scrollview.ScrollOffset() + msg.Y - d.bodyY
		if line == geometry.start && geometry.chevron >= 0 && msg.X == d.bodyX+geometry.chevron {
			d.branch(d.collapsed[d.rows[index].Node.ID])
			d.lastClickIndex = -1
		} else if twice {
			return d, d.attach()
		}
	default:
		if handled, cmd := d.scrollview.Update(msg); handled {
			return d, cmd
		}
	}
	return d, nil
}
func (d *subagentsDialog) SetSize(w, h int) tea.Cmd {
	cmd := d.pickerCore.SetSize(w, h)
	d.renderBody(true)
	d.ensureSelectedVisible()
	return tea.Batch(cmd, d.syncHover())
}
func (d *subagentsDialog) Position() (int, int) { return d.CenterDialog(d.View()) }
func (d *subagentsDialog) View() string         { return d.renderBody(false) }
func (d *subagentsDialog) renderBody(prepare bool) string {
	width, _, inner := d.dialogSize()
	header := RenderDialogHeader("Subagents", inner, styles.DialogTitleStyle)
	// Use the body's actual cell budget, including compact frame padding.
	_, _, bodyInner, _ := d.bodyFrame(styles.DialogStyle, width)
	inner = max(1, bodyInner-d.scrollview.ReservedCols())
	lines, geometry, owners := d.renderRows(inner)
	if prepare {
		d.prepared, d.lineRows = geometry, owners
	}
	if len(lines) == 0 {
		lines = []string{styles.MutedStyle.Render("No subagents in this session.")}
	}
	label := "Use subagents: ON (global)"
	if !d.useSubagents {
		label = "Use subagents: OFF (global)"
	}
	stopLabel := "Stop subtree"
	if d.policyUnavailable {
		label = "Use subagents: unavailable"
	}
	if d.stopUnavailable {
		stopLabel = "Stop subtree: unavailable"
	}
	actions := actionsForKeys("u", label, "enter", "Attach", "s", stopLabel)
	actions[0].Disabled = d.policyUnavailable
	actions[2].Disabled = !d.canStop()
	if d.stopTarget != "" {
		lines = append([]string{styles.MutedStyle.Render(ansi.Hardwrap("Permanently stop subtree "+d.stopIdentity+"? Children stop too. History remains. [y] Confirm [n/esc] Cancel", inner, true))}, lines...)
		actions = nil
	}
	if len(actions) > 1 {
		actions[1].Disabled = d.selectedID() == ""
	}
	for i := range actions {
		actions[i].HideFocusHint = true
		actions[i].HideShortcut = i != 2
	}
	footer := d.RenderActions(inner, actions...)
	if prepare {
		d.PrepareScrollableBody(styles.DialogStyle, width, header, strings.Join(lines, "\n"), footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, width, header, strings.Join(lines, "\n"), footer)
}
func (d *subagentsDialog) canStop() bool {
	if d.stopUnavailable || d.selected < 0 || d.selected >= len(d.rows) {
		return false
	}
	row := d.rows[d.selected]
	return (row.Parent != "" || row.Node.Parent != "") && row.Node.State != subagent.NodeStopped
}
func cleanDetail(text string) string { return strings.Join(strings.Fields(ansi.Strip(text)), " ") }

func (d *subagentsDialog) Cleanup() {
	d.StopAnimations()
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
}
func (d *subagentsDialog) SetCancel(cancel func()) { d.cancel = cancel }
