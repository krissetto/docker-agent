package dialog

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	effortPickerWidthPercent = 40
	// Wide enough that the three help-key bindings fit on a single line.
	effortPickerMinWidth = 46
	effortPickerMaxWidth = 50
)

// effortPickerDialog lets the user pick a reasoning-effort level for the
// current model among the levels that model supports. It backs /effort when
// no level argument is given.
type effortPickerDialog struct {
	BaseDialog

	levels   []effort.Level
	current  effort.Level
	selected int
}

// NewEffortPickerDialog creates an effort picker over the given supported
// levels. current marks the active level and is preselected; pass an empty
// level when the active configuration doesn't map onto a listed level
// (adaptive or token-based budgets).
func NewEffortPickerDialog(levels []effort.Level, current effort.Level) Dialog {
	d := &effortPickerDialog{levels: levels, current: current}
	for i, l := range levels {
		if l == current {
			d.selected = i
			break
		}
	}
	return d
}

func (d *effortPickerDialog) Init() tea.Cmd { return nil }

func (d *effortPickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
		d.BlurActions()
	}
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
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft {
			x, y, w, h := d.BodyScrollBounds()
			if m.X >= x && m.X < x+w && m.Y >= y && m.Y < y+h {
				idx := m.Y - y + d.BodyScrollOffset()
				if idx < len(d.levels) {
					d.selected = idx
				}
				return d, nil
			}
		}
	case tea.MouseWheelMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		if handled, cmd := d.UpdateBodyScroll(msg); handled {
			return d, cmd
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(msg)
		d.EnsureBodyLineVisible(d.selected)
		return d, cmd
	}
	return d, nil
}

func (d *effortPickerDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		return closeDialogCmd()
	case "up", "k", "ctrl+k":
		if d.selected > 0 {
			d.selected--
		}
	case "down", "j", "ctrl+j":
		if d.selected < len(d.levels)-1 {
			d.selected++
		}
	case "home", "g":
		d.selected = 0
	case "end", "G":
		d.selected = max(0, len(d.levels)-1)
	case "enter":
		return d.handleSelection()
	}
	return nil
}

func (d *effortPickerDialog) handleSelection() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.levels) {
		return nil
	}
	return tea.Sequence(
		closeDialogCmd(),
		core.CmdHandler(messages.SetThinkingLevelMsg{Level: d.levels[d.selected].String()}),
	)
}

func (d *effortPickerDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *effortPickerDialog) View() string { return d.renderBody(false) }

func (d *effortPickerDialog) renderBody(prepare bool) string {
	width := d.ComputeDialogWidth(effortPickerWidthPercent, effortPickerMinWidth, effortPickerMaxWidth)
	inner := d.BodyContentWidth(width)

	rows := make([]string, 0, len(d.levels))
	for i, level := range d.levels {
		rows = append(rows, toolcommon.TruncateText(d.renderLevel(level, i == d.selected), inner))
	}

	header := RenderDialogHeader("Select Reasoning Effort", inner, styles.DialogTitleStyle)
	actions := actionsForKeys("enter", "Use effort")
	actions[0].Disabled = d.selected < 0 || d.selected >= len(d.levels)
	footer := d.RenderActions(inner, actions...)
	if prepare {
		d.PrepareScrollableBody(styles.DialogStyle, width, header, lipgloss.JoinVertical(lipgloss.Left, rows...), footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, width, header, lipgloss.JoinVertical(lipgloss.Left, rows...), footer)
}

// renderLevel draws one list entry: the six-cell effort gauge (empty for
// none, matching the sidebar's visual language), the level name, and a
// "(current)" badge on the active level.
func (d *effortPickerDialog) renderLevel(level effort.Level, selected bool) string {
	nameStyle := styles.PaletteUnselectedActionStyle
	currentBadgeStyle := styles.BadgeCurrentStyle
	if selected {
		nameStyle = styles.PaletteSelectedActionStyle
		currentBadgeStyle = currentBadgeStyle.Background(styles.MobyBlue)
	}

	name := nameStyle.Render(" " + level.String() + " ")
	if level == d.current {
		name += currentBadgeStyle.Render("(current)")
	}
	return toolcommon.EffortGauge(level) + " " + name
}

func (d *effortPickerDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	return cmd
}
