// Package messagebar renders optional notices in a stable bottom row.
package messagebar

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Action struct {
	Label   string
	Command tea.Cmd
}

// Message is a plain-text, single-row notice; actions return caller-owned commands.
type Message struct {
	Text     string
	Severity Severity
	Actions  []Action
}

// SetMessageMsg and ClearMessageMsg are handled by the owning event loop.
type SetMessageMsg Message

type ClearMessageMsg struct{}

type actionBounds struct {
	index, start, end int
}

type Model struct {
	width, height int
	message       Message
	text          string
	bounds        []actionBounds
	hovered       int
	selected      int
	focused       bool
	visualDirty   bool
	cacheValid    bool
	cachedView    string
	theme         uint64

	ar         *animation.Runtime
	transition animation.Transition
	alpha      float64
	closing    bool
	closeFrom  float64
}

func New() *Model {
	return &Model{hovered: -1, selected: -1, alpha: 1}
}

func (m *Model) SetSize(width, height int) tea.Cmd {
	defer m.prepareView()
	width, height = max(0, width), max(0, min(1, height))
	if m.width == width && m.height == height {
		return nil
	}
	m.width, m.height = width, height
	if m.width == 0 || m.height == 0 {
		m.StopAnimations()
	}
	m.hovered = -1
	m.layout()
	m.invalidate()
	return nil
}

func (m *Model) Height() int { return m.height }

func (m *Model) SetMessage(message Message) tea.Cmd {
	defer m.prepareView()
	m.message = message
	m.message.Text = singleLine(message.Text)
	m.message.Actions = append([]Action(nil), message.Actions...)
	for i := range m.message.Actions {
		m.message.Actions[i].Label = singleLine(m.message.Actions[i].Label)
	}
	m.focused, m.hovered, m.selected = false, -1, -1
	m.layout()
	m.invalidate()
	return m.startNotice()
}

func (m *Model) HasActions() bool { return !m.closing && len(m.bounds) > 0 }

func (m *Model) SetFocused(focused bool) {
	defer m.prepareView()
	focused = focused && m.HasActions()
	if m.focused == focused {
		return
	}
	m.focused = focused
	if focused {
		m.selected = m.bounds[0].index
	} else {
		m.selected = -1
	}
	m.invalidate()
}

func (m *Model) Focused() bool { return m.focused }

// HitTest uses local terminal cells, including pill padding but not separators.
func (m *Model) HitTest(x, y int) (int, bool) {
	if m.closing || m.height == 0 || y != 0 || x < 0 || x >= m.width {
		return -1, false
	}
	for _, bounds := range m.bounds {
		if x >= bounds.start && x < bounds.end {
			return bounds.index, true
		}
	}
	return -1, false
}

func (m *Model) Update(msg tea.Msg) tea.Cmd {
	defer m.prepareView()
	switch msg := msg.(type) {
	case SetMessageMsg:
		return m.SetMessage(Message(msg))
	case ClearMessageMsg:
		return m.ClearMessage()
	case animation.TickMsg:
		return m.tick(msg)
	case tea.MouseMotionMsg:
		hovered, _ := m.HitTest(msg.X, msg.Y)
		if hovered != m.hovered {
			m.hovered = hovered
			m.invalidate()
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			if index, ok := m.HitTest(msg.X, msg.Y); ok {
				return m.message.Actions[index].Command
			}
		}
	case tea.BlurMsg:
		m.SetFocused(false)
		if m.hovered != -1 {
			m.hovered = -1
			m.invalidate()
		}
	case tea.KeyPressMsg:
		if !m.focused || !m.HasActions() {
			return nil
		}
		switch msg.String() {
		case "enter", "space":
			return m.message.Actions[m.selected].Command
		case "esc":
			m.SetFocused(false)
		case "left", "shift+tab", "right", "tab":
			delta := 1
			if msg.String() == "left" || msg.String() == "shift+tab" {
				delta = -1
			}
			for i, bounds := range m.bounds {
				if bounds.index == m.selected {
					selected := m.bounds[(i+delta+len(m.bounds))%len(m.bounds)].index
					if selected != m.selected {
						m.selected = selected
						m.invalidate()
					}
					break
				}
			}
		}
	}
	return nil
}

func (m *Model) TakeVisualDirty() bool {
	dirty := m.visualDirty
	m.visualDirty = false
	return dirty
}

func (m *Model) invalidate() {
	m.cacheValid = false
	m.visualDirty = true
}

func (m *Model) layout() {
	m.bounds = nil
	m.text = ""
	if m.width == 0 || m.height == 0 {
		m.focused, m.selected = false, -1
		return
	}
	available := m.width
	// Reserve a message cell and separator, then fit only complete action pills.
	actionBudget := available
	if m.message.Text != "" {
		actionBudget = max(0, actionBudget-2)
	}
	actionWidth := 0
	for i, action := range m.message.Actions {
		if action.Label == "" || action.Command == nil {
			continue
		}
		width := ansi.StringWidth(action.Label) + 2
		gap := 0
		if len(m.bounds) > 0 {
			gap = 1
		}
		if actionWidth+gap+width > actionBudget {
			break
		}
		start := actionWidth + gap
		m.bounds = append(m.bounds, actionBounds{index: i, start: start, end: start + width})
		actionWidth = start + width
	}
	textWidth := available
	if actionWidth > 0 {
		textWidth = max(0, available-actionWidth-1)
		for i := range m.bounds {
			m.bounds[i].start += available - actionWidth
			m.bounds[i].end += available - actionWidth
		}
	}
	if textWidth > 0 {
		m.text = ansi.Truncate(m.message.Text, textWidth, "…")
	}
	if !m.HasActions() {
		m.focused, m.selected = false, -1
	} else if m.focused {
		visible := false
		for _, bounds := range m.bounds {
			visible = visible || bounds.index == m.selected
		}
		if !visible {
			m.selected = m.bounds[0].index
		}
	}
}

func (m *Model) View() string {
	if m.cacheValid && m.theme == styles.ThemeGeneration() {
		return m.cachedView
	}
	return m.render()
}

// Cache preparation belongs to the event loop, never to View.
func (m *Model) prepareView() {
	generation := styles.ThemeGeneration()
	if m.cacheValid && m.theme == generation {
		return
	}
	m.cachedView = m.render()
	m.cacheValid, m.theme = true, generation
}

func (m *Model) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	base := styles.NoStyle.Background(styles.Background)
	var out strings.Builder
	out.WriteString(base.Foreground(m.severityColor()).Render(m.text))
	cursor := ansi.StringWidth(m.text)
	for _, bounds := range m.bounds {
		out.WriteString(base.Render(strings.Repeat(" ", bounds.start-cursor)))
		pill := styles.NoStyle.Padding(0, 1).Bold(true).Foreground(styles.TextPrimary).Background(styles.BackgroundAlt)
		if m.focused && bounds.index == m.selected {
			pill = pill.Underline(true)
		}
		text := pill.Render(m.message.Actions[bounds.index].Label)
		if bounds.index == m.hovered {
			text = styles.HoverText(text, 1, styles.TextPrimary)
		}
		out.WriteString(text)
		cursor = bounds.end
	}
	notice := out.String()
	if m.alpha < 1 {
		notice = styles.FadeLine(notice, m.alpha)
	}
	out.Reset()
	out.WriteString(notice)
	out.WriteString(base.Render(strings.Repeat(" ", m.width-cursor)))
	return out.String()
}

func singleLine(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(text))
	return strings.Join(strings.Fields(text), " ")
}
