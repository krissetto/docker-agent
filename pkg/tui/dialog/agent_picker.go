package dialog

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const agentCardHeight = 8

type agentPickerDialog struct {
	BaseDialog
	agents                       []runtime.AgentDetails
	current, workingDir          string
	selected, columns, cardWidth int
}

func NewAgentPickerDialog(agents []runtime.AgentDetails, current, workingDir string) Dialog {
	d := &agentPickerDialog{agents: agents, current: current, workingDir: workingDir}
	for i, a := range agents {
		if a.Name == current {
			d.selected = i
			break
		}
	}
	return d
}

func (d *agentPickerDialog) Init() tea.Cmd            { return nil }
func (d *agentPickerDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }
func (d *agentPickerDialog) View() string             { return d.renderBody(false) }
func (d *agentPickerDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	d.revealSelection()
	return cmd
}

func (d *agentPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if preparesDialogBody(msg) {
		defer d.renderBody(true)
	}
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if action, handled := d.HandleActionKey(k); handled {
			if action.Code == 0 {
				return d, nil
			}
			msg = action
		}
	}
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		return d, d.SetSize(m.Width, m.Height)
	case tea.MouseWheelMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		if handled, cmd := d.UpdateBodyScroll(msg); handled {
			return d, cmd
		}
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft {
			d.BlurActions()
			x, y, w, h := d.BodyScrollBounds()
			if m.X >= x && m.X < x+w && m.Y >= y && m.Y < y+h {
				column := (m.X - x) / (d.cardWidth + 1)
				line := m.Y - y + d.BodyScrollOffset()
				index := line/(agentCardHeight+1)*d.columns + column
				if column < d.columns && (m.X-x)%(d.cardWidth+1) < d.cardWidth && line%(agentCardHeight+1) < agentCardHeight && index < len(d.agents) {
					d.selected = index
					return d, d.choose()
				}
			}
		}
	case tea.KeyPressMsg:
		if cmd := HandleQuit(m); cmd != nil {
			return d, cmd
		}
		switch m.String() {
		case "esc", "q":
			return d, closeDialogCmd()
		case "left", "h":
			d.selected = max(0, d.selected-1)
		case "right", "l":
			d.selected = min(len(d.agents)-1, d.selected+1)
		case "up", "k", "ctrl+p":
			d.selected = max(0, d.selected-d.columns)
		case "down", "j", "ctrl+n":
			d.selected = min(len(d.agents)-1, d.selected+d.columns)
		case "home", "g":
			d.selected = 0
		case "end", "G":
			d.selected = max(0, len(d.agents)-1)
		case "pgup", "pgdown":
			_, _, _, h := d.BodyScrollBounds()
			step := max(1, h/(agentCardHeight+1)) * d.columns
			if m.String() == "pgup" {
				d.selected = max(0, d.selected-step)
			} else {
				d.selected = min(len(d.agents)-1, d.selected+step)
			}
		case "enter":
			return d, d.choose()
		}
		d.revealSelection()
	}
	return d, nil
}

func (d *agentPickerDialog) revealSelection() {
	if d.columns > 0 {
		line := d.selected / d.columns * (agentCardHeight + 1)
		d.EnsureBodyLineVisible(line + agentCardHeight - 1)
		d.EnsureBodyLineVisible(line)
	}
}

func (d *agentPickerDialog) choose() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.agents) {
		return nil
	}
	return tea.Sequence(closeDialogCmd(), core.CmdHandler(messages.NewAgentSessionMsg{AgentName: d.agents[d.selected].Name, WorkingDir: d.workingDir}))
}

func (d *agentPickerDialog) renderBody(prepare bool) string {
	width := d.ComputeDialogWidth(90, 36, 140)
	inner := d.BodyContentWidth(width)
	d.columns = max(1, (inner+1)/33)
	d.cardWidth = max(1, (inner-(d.columns-1))/d.columns)
	var rows []string
	for start := 0; start < len(d.agents); start += d.columns {
		var cards []string
		for i := start; i < min(start+d.columns, len(d.agents)); i++ {
			cards = append(cards, d.renderCard(i))
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cards...))
	}
	body := strings.Join(rows, "\n\n")
	if len(d.agents) == 0 {
		body = styles.MutedStyle.Render("No agents available")
	}
	header := RenderDialogHeader(fmt.Sprintf("Select Agent (%d)", len(d.agents)), inner, styles.DialogTitleStyle)
	actions := actionsForKeys("enter", "New session")
	actions[0].Disabled = len(d.agents) == 0
	footer := d.RenderActions(inner, actions...)
	if prepare {
		d.PrepareScrollableBody(styles.DialogStyle, width, header, body, footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *agentPickerDialog) renderCard(index int) string {
	a := d.agents[index]
	width := max(1, d.cardWidth-4)
	name := a.Name
	if a.Name == d.current {
		name += " (current)"
	}
	model := a.Model
	if a.Provider != "" {
		model = a.Provider + "/" + model
	}
	lines := []string{styles.AgentAccentStyleFor(a.Name).Bold(true).Render(ansi.Truncate(name, width, "…")), styles.MutedStyle.Render(ansi.Truncate(model, width, "…"))}
	thinking := ""
	if a.Thinking != "" {
		thinking = "Thinking: " + a.Thinking
	}
	lines = append(lines, styles.MutedStyle.Render(ansi.Truncate(thinking, width, "…")))
	desc := strings.Split(ansi.Hardwrap(strings.Join(strings.Fields(a.Description), " "), width, true), "\n")
	for len(desc) < 3 {
		desc = append(desc, "")
	}
	if len(desc) > 3 {
		desc[2] = ansi.Truncate(desc[2]+"…", width, "…")
	}
	lines = append(lines, desc[:3]...)
	border := styles.BorderSecondary
	if index == d.selected {
		border = styles.Highlight
	}
	for i, line := range lines {
		lines[i] = " " + line + strings.Repeat(" ", max(0, width-lipgloss.Width(line))) + " "
	}
	card := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Render(strings.Join(lines, "\n"))
	// One unowned column separates neighboring cards.
	if index%d.columns < d.columns-1 {
		card = lipgloss.NewStyle().MarginRight(1).Render(card)
	}
	return card
}
