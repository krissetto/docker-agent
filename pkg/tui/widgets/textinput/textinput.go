// Package textinput is a single-line Bubble Tea adapter over the pure text core.
package textinput

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/text"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

type EchoMode int

const (
	EchoNormal EchoMode = iota
	EchoPassword
	EchoNone
)

type CursorStyle = cursor.Style
type Styles struct {
	Focused, Blurred StyleState
	Cursor           CursorStyle
}
type StyleState struct{ Text, Placeholder, Suggestion, Prompt lipgloss.Style }
type ValidateFunc func(string) error

func DefaultStyles(isDark bool) Styles {
	muted := lipgloss.Color("240")
	if !isDark {
		muted = lipgloss.Color("245")
	}
	s := StyleState{Placeholder: lipgloss.NewStyle().Foreground(muted), Suggestion: lipgloss.NewStyle().Foreground(muted)}
	return Styles{Focused: s, Blurred: s, Cursor: CursorStyle{Blink: true, BlinkSpeed: 530 * time.Millisecond}}
}
func DefaultDarkStyles() Styles  { return DefaultStyles(true) }
func DefaultLightStyles() Styles { return DefaultStyles(false) }

type Model struct {
	Prompt, Placeholder string
	CharLimit           int
	EchoMode            EchoMode
	EchoCharacter       rune
	KeyMap              KeyMap
	Err                 error
	Validate            ValidateFunc
	state               text.State
	width               int
	focused, virtual    bool
	styles              Styles
	blink               cursor.Model
	generation          uint64
}

func New() Model {
	return Model{Prompt: "> ", EchoCharacter: '*', KeyMap: DefaultKeyMap(), state: text.New(text.Options{}), virtual: true, styles: DefaultDarkStyles()}
}
func Blink() tea.Msg { return cursor.Blink() }
func (m *Model) config() text.Config {
	width := m.width + 1
	if m.width <= 0 {
		width = max(1, ansi.StringWidth(m.Value())+1)
	}
	c := text.Config{Width: width, Height: 1}
	if m.EchoMode == EchoPassword {
		c.Mask = m.EchoCharacter
		if c.Mask == 0 {
			c.Mask = '*'
		}
	}
	c.EchoNone = m.EchoMode == EchoNone
	return c
}
func (m *Model) active() StyleState {
	if m.focused {
		return m.styles.Focused
	}
	return m.styles.Blurred
}
func (m *Model) normalize() {
	m.state.SetCharLimit(m.CharLimit)
	m.state.NormalizeViewport(m.config())
	if m.Validate != nil {
		m.Err = m.Validate(m.Value())
	}
}
func (m *Model) Value() string { return m.state.Value() }
func (m *Model) SetValue(value string) {
	oldValue, pos := m.Value(), m.Position()
	m.state.SetCharLimit(m.CharLimit)
	m.state.SetValue(strings.ReplaceAll(value, "\t", " "))
	if oldValue == "" && pos == 0 || pos > len([]rune(m.Value())) {
		pos = len([]rune(m.Value()))
	}
	m.state.SetCursor(text.Position{Column: pos})
	m.normalize()
}
func (m *Model) InsertString(value string) {
	m.state.SetCharLimit(m.CharLimit)
	m.state.Insert(strings.ReplaceAll(value, "\t", " "))
	m.normalize()
}
func (m *Model) Reset()     { m.SetValue("") }
func (m *Model) Width() int { return m.width }
func (m *Model) SetWidth(width int) {
	if m.width != max(0, width) {
		m.generation++
	}
	m.width = max(0, width)
	m.normalize()
}
func (m *Model) Position() int { return m.state.Column() }
func (m *Model) SetCursor(position int) {
	m.state.SetCursor(text.Position{Column: position})
	m.normalize()
}
func (m *Model) CursorStart() { m.SetCursor(0) }
func (m *Model) CursorEnd()   { m.SetCursor(len([]rune(m.Value()))) }
func (m *Model) PositionAtCell(cell int) int {
	return m.state.PositionAtCell(m.config(), 0, cell).Column
}
func (m *Model) Layout() text.Layout { return m.state.Layout(m.config()) }
func (m *Model) Focused() bool       { return m.focused }
func (m *Model) Focus() tea.Cmd {
	m.focused = true
	m.generation++
	m.normalize()
	return m.blink.Reset(m.styles.Cursor)
}
func (m *Model) Blur()                         { m.focused = false; m.generation++; m.blink.Stop() }
func (m *Model) Styles() Styles                { return m.styles }
func (m *Model) SetStyles(styles Styles)       { m.styles = styles; m.generation++ }
func (m *Model) VirtualCursor() bool           { return m.virtual }
func (m *Model) SetVirtualCursor(enabled bool) { m.virtual = enabled; m.generation++ }

// Revision covers mutation methods; direct exported-field assignments must also
// invalidate an owning presentation cache.
func (m *Model) Revision() uint64        { return m.state.Revision() + m.generation }
func (m *Model) ContentRevision() uint64 { return m.state.ContentRevision() }
func (m *Model) Cursor() *tea.Cursor {
	if m.virtual || !m.focused {
		return nil
	}
	cell := m.state.CursorCell(m.config())
	c := tea.NewCursor(ansi.StringWidth(m.Prompt)+cell.Column-m.state.ScrollXOffset(), 0)
	c.Color = m.styles.Cursor.Color
	c.Shape = m.styles.Cursor.Shape
	c.Blink = m.styles.Cursor.Blink
	return c
}

type pasteMsg string

func Paste() tea.Msg {
	v, err := clipboard.ReadAll()
	if err != nil {
		return err
	}
	return pasteMsg(v)
}
func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	if !m.focused {
		return m, nil
	}
	switch v := msg.(type) {
	case tea.PasteMsg:
		m.InsertString(v.Content)
		return m, m.blink.Reset(m.styles.Cursor)
	case pasteMsg:
		m.InsertString(string(v))
		return m, m.blink.Reset(m.styles.Cursor)
	case tea.KeyPressMsg:
		m.state.SetCharLimit(m.CharLimit)
		if key.Matches(v, m.KeyMap.Paste) {
			return m, Paste
		}
		// Masked inputs must not disclose word boundaries through cursor movement
		// or deletion; these bindings act on the entire hidden remainder.
		if m.EchoMode != EchoNormal {
			masked := []struct {
				binding key.Binding
				kind    text.ActionKind
			}{
				{m.KeyMap.WordForward, text.MoveEnd}, {m.KeyMap.WordBackward, text.MoveHome},
				{m.KeyMap.DeleteWordForward, text.DeleteToEnd}, {m.KeyMap.DeleteWordBackward, text.DeleteToStart},
			}
			for _, a := range masked {
				if key.Matches(v, a.binding) {
					m.state.Apply(text.Action{Kind: a.kind, Config: m.config()})
					m.normalize()
					return m, m.blink.Reset(m.styles.Cursor)
				}
			}
		}
		actions := []struct {
			binding key.Binding
			kind    text.ActionKind
		}{
			{m.KeyMap.CharacterForward, text.MoveRight}, {m.KeyMap.CharacterBackward, text.MoveLeft},
			{m.KeyMap.WordForward, text.MoveWordRight}, {m.KeyMap.WordBackward, text.MoveWordLeft},
			{m.KeyMap.LineStart, text.MoveHome}, {m.KeyMap.LineEnd, text.MoveEnd},
			{m.KeyMap.DeleteCharacterBackward, text.DeleteBackward}, {m.KeyMap.DeleteCharacterForward, text.DeleteForward},
			{m.KeyMap.DeleteWordBackward, text.DeleteWordBackward}, {m.KeyMap.DeleteWordForward, text.DeleteWordForward},
			{m.KeyMap.DeleteBeforeCursor, text.DeleteToStart}, {m.KeyMap.DeleteAfterCursor, text.DeleteToEnd},
		}
		for _, a := range actions {
			if key.Matches(v, a.binding) {
				m.state.Apply(text.Action{Kind: a.kind, Config: m.config()})
				m.normalize()
				return m, m.blink.Reset(m.styles.Cursor)
			}
		}
		if v.Text != "" && v.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModMeta|tea.ModSuper) == 0 {
			m.InsertString(v.Text)
			return m, m.blink.Reset(m.styles.Cursor)
		}
		return m, nil
	default:
		before := m.blink.Visible(m.styles.Cursor)
		cmd := m.blink.Update(msg, m.styles.Cursor)
		if before != m.blink.Visible(m.styles.Cursor) {
			m.generation++
		}
		return m, cmd
	}
}

// View never moves the insertion point or horizontal viewport.
func (m Model) View() string {
	l := m.Layout()
	s := m.active()
	placeholder := m.Value() == "" && m.Placeholder != ""
	if placeholder {
		preview := text.New(text.Options{})
		preview.SetValue(m.Placeholder)
		c := m.config()
		c.Mask = 0
		c.EchoNone = false
		l.Rows = preview.Layout(c).Rows
	}
	var body, span strings.Builder
	style := s.Text
	if placeholder {
		style = s.Placeholder
	}
	visible := m.focused && m.virtual && m.blink.Visible(m.styles.Cursor)
	cursorDrawn := false
	cells := 0
	for _, run := range l.Rows[0].Runs {
		value := run.Text
		if visible && run.Cell == l.Cursor.Column && !cursorDrawn {
			if span.Len() > 0 {
				body.WriteString(style.Render(span.String()))
				span.Reset()
			}
			body.WriteString(style.Render(cursor.Render(value, m.styles.Cursor)))
			cursorDrawn = true
		} else {
			span.WriteString(value)
		}
		cells += run.Width
	}
	if span.Len() > 0 {
		body.WriteString(style.Render(span.String()))
	}
	// Preserve the trailing insertion cell even when the text fills Width().
	if !cursorDrawn && visible {
		body.WriteString(style.Render(cursor.Render(" ", m.styles.Cursor)))
		cells++
	}
	target := cells
	if m.width > 0 {
		target = m.width + 1 + l.ScrollX
	}
	if cells < target {
		body.WriteString(style.Render(strings.Repeat(" ", target-cells)))
	}
	result := body.String()
	if m.width > 0 {
		result = ansi.Cut(result, l.ScrollX, l.ScrollX+m.width+1)
	}
	return s.Prompt.Render(m.Prompt) + result
}
