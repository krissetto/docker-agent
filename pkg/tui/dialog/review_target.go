package dialog

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
	"github.com/docker/docker-agent/pkg/tui/widgets/textinput"
)

const (
	reviewTargetWidthPercent = 60
	reviewTargetMinWidth     = 48
	reviewTargetMaxWidth     = 90
)

// ReviewTarget is one recipient choice; the host decides eligibility and only
// lists targets it can resolve to an authorized handle.
type ReviewTarget struct {
	ID       string
	Label    string
	Detail   string
	Disabled string // why the target can't receive input; empty when eligible
	New      bool   // "New session…" entry
}

type ReviewTargetConfig struct {
	DialogID string
	Title    string
	Targets  []ReviewTarget
	Default  string
	// More and Partial describe bounded loading: More offers "Load more", Partial explains a capped list.
	More    bool
	Partial string
	Context any
}

type ReviewTargetResultMsg struct {
	DialogID  string
	Target    ReviewTarget
	Cancelled bool
	Context   any
}

// ReviewTargetMoreMsg asks the host for the next bounded page.
type ReviewTargetMoreMsg struct {
	DialogID string
	Page     int
	Context  any
}

// ReviewTargetPageMsg appends a page from the host to an open picker.
type ReviewTargetPageMsg struct {
	DialogID string
	Targets  []ReviewTarget
	More     bool
	Partial  string
}

func (ReviewTargetPageMsg) BroadcastToDialogs() {}

type reviewTargetDialog struct {
	BaseDialog
	config   ReviewTargetConfig
	filter   textinput.Model
	visible  []int // indices into config.Targets, plus -1 for "Load more"
	selected int
	page     int
	loading  bool
}

func NewReviewTargetDialog(config ReviewTargetConfig) Dialog {
	in := textinput.New()
	in.Prompt = "/ "
	in.Placeholder = "search recipients…"
	in.SetStyles(styles.DialogInputStyle)
	d := &reviewTargetDialog{config: config, filter: in}
	d.filter.Focus()
	d.refilter()
	for i, idx := range d.visible {
		if idx >= 0 && config.Targets[idx].ID == config.Default {
			d.selected = i
		}
	}
	return d
}

func (d *reviewTargetDialog) Init() tea.Cmd { return nil }

func (d *reviewTargetDialog) refilter() {
	q := strings.ToLower(strings.TrimSpace(d.filter.Value()))
	d.visible = d.visible[:0]
	for i, t := range d.config.Targets {
		if q == "" || strings.Contains(strings.ToLower(t.Label+" "+t.Detail), q) {
			d.visible = append(d.visible, i)
		}
	}
	if d.config.More {
		d.visible = append(d.visible, -1)
	}
	d.selected = max(0, min(d.selected, len(d.visible)-1))
}

func (d *reviewTargetDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if preparesDialogBody(msg) {
		defer d.renderBody(true)
	}
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(m.Width, m.Height)
		return d, cmd
	case ReviewTargetPageMsg:
		if m.DialogID == d.config.DialogID {
			d.config.Targets = append(d.config.Targets, m.Targets...)
			d.config.More, d.config.Partial, d.loading = m.More, m.Partial, false
			d.refilter()
		}
		return d, nil
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft {
			x, y, w, h := d.BodyScrollBounds()
			if m.X >= x && m.X < x+w && m.Y >= y && m.Y < y+h {
				i := m.Y - y + d.BodyScrollOffset()
				if i < len(d.visible) {
					if i == d.selected {
						cmd := d.choose()
						return d, cmd
					}
					d.selected = i
				}
			}
		}
		return d, nil
	case tea.MouseWheelMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		_, cmd := d.UpdateBodyScroll(msg)
		return d, cmd
	case tea.PasteMsg:
		var cmd tea.Cmd
		d.filter, cmd = d.filter.Update(m)
		d.refilter()
		return d, cmd
	case tea.KeyPressMsg:
		if cmd := HandleQuit(m); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(m)
		d.EnsureBodyLineVisible(d.selected)
		return d, cmd
	}
	return d, nil
}

func (d *reviewTargetDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		if d.filter.Value() != "" {
			d.filter.Reset()
			d.refilter()
			return nil
		}
		return d.cancel()
	case "up", "ctrl+k":
		d.selected = max(0, d.selected-1)
	case "down", "ctrl+j":
		d.selected = min(len(d.visible)-1, d.selected+1)
	case "pgup":
		d.selected = max(0, d.selected-max(1, d.bodyHeight))
	case "pgdown":
		d.selected = min(len(d.visible)-1, d.selected+max(1, d.bodyHeight))
	case "home":
		d.selected = 0
	case "end":
		d.selected = max(0, len(d.visible)-1)
	case "enter":
		return d.choose()
	default:
		var cmd tea.Cmd
		before := d.filter.Value()
		d.filter, cmd = d.filter.Update(msg)
		if d.filter.Value() != before {
			d.selected = 0
			d.refilter()
		}
		return cmd
	}
	return nil
}

func (d *reviewTargetDialog) choose() tea.Cmd {
	if len(d.visible) == 0 {
		return nil
	}
	idx := d.visible[d.selected]
	if idx < 0 {
		if d.loading {
			return nil
		}
		d.loading = true
		d.page++
		return core.CmdHandler(ReviewTargetMoreMsg{DialogID: d.config.DialogID, Page: d.page, Context: d.config.Context})
	}
	target := d.config.Targets[idx]
	if target.Disabled != "" || !d.claimResponse() {
		return nil
	}
	return tea.Sequence(core.CmdHandler(CloseDialogMsg{}), core.CmdHandler(ReviewTargetResultMsg{DialogID: d.config.DialogID, Target: target, Context: d.config.Context}))
}

func (d *reviewTargetDialog) cancel() tea.Cmd {
	if !d.claimResponse() {
		return nil
	}
	return tea.Sequence(core.CmdHandler(CloseDialogMsg{}), core.CmdHandler(ReviewTargetResultMsg{DialogID: d.config.DialogID, Cancelled: true, Context: d.config.Context}))
}

// CancelDialogCmd keeps outside clicks and Esc on the same result path.
func (d *reviewTargetDialog) CancelDialogCmd() tea.Cmd { return d.cancel() }

func (d *reviewTargetDialog) Position() (row, col int) { return d.CenterDialog(d.View()) }

func (d *reviewTargetDialog) View() string { return d.renderBody(false) }

func (d *reviewTargetDialog) renderBody(prepare bool) string {
	width := d.ComputeDialogWidth(reviewTargetWidthPercent, reviewTargetMinWidth, reviewTargetMaxWidth)
	inner := d.BodyContentWidth(width)
	d.filter.SetWidth(max(1, inner-4))
	header := RenderDialogHeader(d.config.Title, inner, styles.DialogTitleStyle) + "\n" + d.filter.View()
	footer := d.RenderActionKeys(inner, "enter", "choose", "esc", "cancel")
	body := d.bodyContent(inner)
	if prepare {
		d.PrepareScrollableBody(styles.DialogStyle, width, header, body, footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, width, header, body, footer)
}

func (d *reviewTargetDialog) bodyContent(inner int) string {
	if len(d.visible) == 0 {
		return styles.DialogContentStyle.Italic(true).Foreground(styles.TextMuted).Width(inner).Render("No matching recipients")
	}
	rows := make([]string, 0, len(d.visible)+1)
	for i, idx := range d.visible {
		selected := i == d.selected
		if idx < 0 {
			label := "Load more…"
			if d.loading {
				label = "Loading…"
			}
			rows = append(rows, d.renderRow(label, "", "", selected, inner))
			continue
		}
		t := d.config.Targets[idx]
		rows = append(rows, d.renderRow(t.Label, t.Detail, t.Disabled, selected, inner))
	}
	if d.config.Partial != "" {
		rows = append(rows, styles.MutedStyle.Italic(true).Render(toolcommon.TruncateText(d.config.Partial, inner)))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (d *reviewTargetDialog) renderRow(name, detail, disabled string, selected bool, width int) string {
	nameStyle, descStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	if selected {
		nameStyle, descStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
	}
	if disabled != "" {
		detail = disabled
		nameStyle = nameStyle.Foreground(styles.TextMuted)
	}
	left := nameStyle.Render(" " + name + " ")
	right := descStyle.Render(" " + detail + " ")
	gap := max(0, width-lipgloss.Width(left))
	return toolcommon.TruncateText(left+lipgloss.PlaceHorizontal(gap, lipgloss.Right, right, lipgloss.WithWhitespaceStyle(descStyle)), width)
}

func (d *reviewTargetDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	d.renderBody(true)
	return cmd
}

// CursorInput exposes the focused text field's cursor to the root blink driver.
// It returns nil when no text field in this dialog has focus.
func (d *reviewTargetDialog) CursorInput() cursor.Input {
	if d.filter.Focused() {
		return &d.filter
	}
	return nil
}
