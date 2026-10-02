package ui

import (
	"github.com/charmbracelet/x/ansi"
	"strings"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

// SubagentPicker keeps selection by canonical identity, never by display name or row index.
type SubagentPicker struct {
	PolicyUnavailable, StopUnavailable bool
	stopIdentity                       string
	UseSubagents                       bool
	StopTarget                         subagent.NodeID
	actionFocus                        int // 0: tree, 1: toggle, 2: Attach, 3: stop subtree
	rows                               []subagentview.Row
	nodes                              []subagent.NodeSnapshot
	collapsed                          map[subagent.NodeID]bool
	selected                           subagent.NodeID
	current                            subagent.NodeID
	root                               subagent.NodeID
	sessionID                          string
	attached                           subagent.NodeID
}

func NewSubagentPicker(snapshot subagent.Snapshot, sessionID string, attached subagent.NodeID) *SubagentPicker {
	p := &SubagentPicker{UseSubagents: true, collapsed: make(map[subagent.NodeID]bool), sessionID: sessionID, attached: attached}
	p.Update(snapshot)
	return p
}

func (p *SubagentPicker) Update(snapshot subagent.Snapshot) {
	if p.root == "" {
		if current, ok := subagentview.Root(snapshot, p.sessionID, p.attached); ok {
			p.current, p.selected = current.Node.ID, current.Node.ID
			for _, root := range snapshot.Nodes {
				if _, contains := subagentview.Find([]subagent.NodeSnapshot{root}, current.Node.ID); contains {
					p.root = root.Node.ID
					break
				}
			}
		}
	}
	p.nodes = nil
	if root, ok := subagentview.Find(snapshot.Nodes, p.root); ok {
		p.nodes = subagentview.Sorted([]subagent.NodeSnapshot{root})
	}
	p.rebuild()
}

func (p *SubagentPicker) rebuild() {
	p.rows = subagentview.Rows(p.nodes, p.collapsed)
	// A removed selection must not silently retarget Enter to a different session.
	if p.index() < 0 {
		p.selected = ""
	}
}

func (p *SubagentPicker) index() int {
	for i, row := range p.rows {
		if row.Node.ID == p.selected {
			return i
		}
	}
	return -1
}

func (p *SubagentPicker) Current() (subagent.Node, bool) {
	if i := p.index(); i >= 0 {
		return p.rows[i].Node, true
	}
	return subagent.Node{}, false
}

func (p *SubagentPicker) Navigate(key KeyType) {
	if p.actionFocus > 0 {
		return
	}
	if len(p.rows) == 0 {
		return
	}
	i := p.index()
	if i < 0 {
		i = 0
	} else {
		switch key {
		case KeyUp:
			i = (i + len(p.rows) - 1) % len(p.rows)
		case KeyDown:
			i = (i + 1) % len(p.rows)
		case KeyLeft:
			row := p.rows[i]
			if row.Branch && !p.collapsed[row.Node.ID] {
				p.collapsed[row.Node.ID] = true
				p.rebuild()
				return
			}
			if row.Parent != "" {
				p.selected = row.Parent
			}
			return
		case KeyRight:
			row := p.rows[i]
			if row.Branch {
				if p.collapsed[row.Node.ID] {
					delete(p.collapsed, row.Node.ID)
					p.rebuild()
					return
				}
				i = min(len(p.rows)-1, i+1)
			}
		}
	}
	switch key {
	case KeyHome:
		i = 0
	case KeyEnd:
		i = len(p.rows) - 1
	}
	p.selected = p.rows[i].Node.ID
}

func (p *SubagentPicker) Render(width, height int) []string {
	if width <= 0 || height <= 0 {
		return nil
	}
	var lines []string
	if height >= 3 {
		lines = append(lines, Truncate(StBold().Render("Subagents"), width))
	}
	if p.StopTarget != "" && height >= 4 {
		identity := strings.Split(ansi.Hardwrap("Stop subtree "+p.stopIdentity+"?", width, true), "\n")
		lines = append(lines, identity[:min(len(identity), height-2)]...)
	}
	budget := height - len(lines)
	if height >= 3 {
		budget--
	}
	missing := p.selected == "" && len(p.rows) > 0
	if missing && height >= 4 {
		budget--
	}
	if len(p.rows) == 0 {
		lines = append(lines, Truncate(StMuted().Render("No subagents in this session."), width))
	} else {
		start := max(0, p.index()-budget+1)
		for i := start; i < min(len(p.rows), start+budget); i++ {
			row := p.rows[i]
			marker := "  "
			if row.Branch {
				marker = "▾ "
				if p.collapsed[row.Node.ID] {
					marker = "▸ "
				}
			}
			name := strings.Join(strings.Fields(row.Node.DisplayName()), " ")
			if name == "" {
				name = "Agent"
			}
			guides := row.Guides
			if row.Depth == 0 {
				guides = ""
			}
			// Deep trees still leave room for the selected agent's name.
			if DisplayWidth(guides) > width/3 {
				guides = "… "
			}
			line := guides + marker + styles.AgentIdentityStyle(row.Node.Agent, false).Render(name)
			if row.Node.State != "" {
				line += " · " + string(row.Node.State)
			}
			if row.Node.NeedsAttention {
				line += " !"
			}
			if row.Node.ID == p.current {
				line += " (current)"
			}
			line = Truncate(line, width)
			if row.Node.ID == p.selected {
				line = lipglossSelected(line, width)
			}
			lines = append(lines, line)
		}
	}
	if missing && height >= 4 {
		lines = append(lines, Truncate(StMuted().Render("Selection no longer available."), width))
	}
	if height >= 3 {
		label := "Use subagents: ON (global)"
		if !p.UseSubagents {
			label = "Use subagents: OFF (global)"
		}
		if p.PolicyUnavailable {
			label = "Use subagents: unavailable"
		}
		toggle, attach, stop := "[ "+label+" ]", "[ Attach ]", "[ s: Stop subtree ]"
		if p.StopUnavailable {
			stop = "[ Stop subtree: unavailable ]"
		}
		if p.actionFocus == 1 {
			toggle = StBold().Render(toggle)
		}
		if p.actionFocus == 2 {
			attach = StBold().Render(attach)
		}
		if p.actionFocus == 3 {
			stop = StBold().Render(stop)
		}
		footer := toggle + "  " + attach + "  " + stop
		if p.StopTarget != "" {
			footer = "Permanently stop subtree? [y] Confirm [esc] Cancel; history remains"
		}
		lines = append(lines, Truncate(footer, width))
	}
	return lines
}

// HandleActionKey returns true only when the saved policy toggle is activated.
// Attach retains the existing Enter route; tree navigation is otherwise unchanged.
func (p *SubagentPicker) HandleActionKey(key Key) bool {
	if key.Typ == KeyRune && string(key.Runes) == "u" {
		return !p.PolicyUnavailable
	}
	if key.Typ == KeyTab || key.Typ == KeyShiftTab {
		delta := 1
		if key.Typ == KeyShiftTab {
			delta = 3
		}
		p.actionFocus = (p.actionFocus + delta) % 4
		return false
	}
	if p.actionFocus > 0 && (key.Typ == KeyLeft || key.Typ == KeyRight) {
		if key.Typ == KeyRight {
			p.actionFocus = p.actionFocus%3 + 1
		} else {
			p.actionFocus = (p.actionFocus+1)%3 + 1
		}
		return false
	}
	return !p.PolicyUnavailable && key.Typ == KeyEnter && p.actionFocus == 1
}

// HandleStopKey consumes a separate, explicit confirmation before any stop.
// The returned canonical target is pinned when prompting, not on confirmation.
func (p *SubagentPicker) HandleStopKey(key Key) (target subagent.NodeID, handled bool) {
	if p.StopUnavailable {
		return "", key.Typ == KeyRune && string(key.Runes) == "s" || key.Typ == KeyEnter && p.actionFocus == 3
	}
	if p.StopTarget != "" {
		switch {
		case key.Typ == KeyRune && string(key.Runes) == "y":
			target, p.StopTarget = p.StopTarget, ""
			if _, ok := subagentview.Find(p.nodes, target); !ok {
				target = ""
			}
			return target, true
		case key.Typ == KeyEsc || key.Typ == KeyRune && (string(key.Runes) == "n" || string(key.Runes) == "q"):
			p.StopTarget = ""
		}
		return "", true
	}
	if key.Typ == KeyRune && string(key.Runes) == "s" || key.Typ == KeyEnter && p.actionFocus == 3 {
		if i := p.index(); i >= 0 {
			row := p.rows[i]
			if (row.Parent != "" || row.Node.Parent != "") && row.Node.State != subagent.NodeStopped {
				p.StopTarget = row.Node.ID
				p.stopIdentity = strings.Join(strings.Fields(ansi.Strip(row.Node.DisplayName())), " ") + " (" + strings.Join(strings.Fields(ansi.Strip(string(row.Node.ID))), " ") + ")"
			}
		}
		return "", true
	}
	return "", false
}

// PolicyActionFocused prevents a disabled policy button from falling through to Attach.
func (p *SubagentPicker) PolicyActionFocused() bool { return p.actionFocus == 1 }
