// Package textarea adapts the pure text editor to Bubble Tea and Lip Gloss.
package textarea

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"

	"github.com/docker/docker-agent/pkg/tui/text"
	"github.com/docker/docker-agent/pkg/tui/widgets/cursor"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

type CursorStyle = cursor.Style
type LineInfo = text.LineInfo
type Styles struct {
	Focused, Blurred StyleState
	Cursor           CursorStyle
}
type StyleState struct {
	Base, Text, LineNumber, CursorLineNumber, CursorLine, EndOfBuffer, Placeholder, Prompt, Selection lipgloss.Style
}

func DefaultStyles(isDark bool) Styles {
	muted := lipgloss.Color("240")
	if !isDark {
		muted = lipgloss.Color("245")
	}
	s := StyleState{Placeholder: lipgloss.NewStyle().Foreground(muted), LineNumber: lipgloss.NewStyle().Foreground(muted), EndOfBuffer: lipgloss.NewStyle().Foreground(muted), Selection: lipgloss.NewStyle().Reverse(true)}
	return Styles{Focused: s, Blurred: s, Cursor: CursorStyle{Blink: true, BlinkSpeed: 530 * time.Millisecond}}
}
func DefaultDarkStyles() Styles  { return DefaultStyles(true) }
func DefaultLightStyles() Styles { return DefaultStyles(false) }

type Model struct {
	Prompt, Placeholder                                         string
	ShowLineNumbers, DynamicHeight                              bool
	CharLimit, MaxHeight, MaxWidth, MinHeight, MaxContentHeight int
	KeyMap                                                      KeyMap
	state                                                       text.State
	width, height                                               int
	focused, virtual                                            bool
	styles                                                      Styles
	blink                                                       cursor.Model
	generation                                                  uint64
}

func New() Model {
	m := Model{Prompt: "┃ ", ShowLineNumbers: true, MaxHeight: 99, MaxWidth: 500, MinHeight: 1, KeyMap: DefaultKeyMap(), state: text.New(text.Options{Multiline: true}), height: 6, virtual: true, styles: DefaultDarkStyles()}
	m.SetWidth(40)
	return m
}
func Blink() tea.Msg { return cursor.Blink() }
func (m *Model) config() text.Config {
	return text.Config{Width: m.width, Height: m.height, Wrap: true, TabWidth: 4}
}
func (m *Model) active() StyleState {
	if m.focused {
		return m.styles.Focused
	}
	return m.styles.Blurred
}
func (m *Model) gutterWidth() int {
	if !m.ShowLineNumbers {
		return 0
	}
	return len(strconv.Itoa(max(1, m.MaxHeight))) + 2
}
func (m *Model) normalize() {
	m.state.SetCharLimit(m.CharLimit)
	if m.DynamicHeight {
		h := m.state.VisualLineCount(m.config())
		if m.MaxContentHeight > 0 {
			h = min(h, m.MaxContentHeight)
		}
		m.height = max(max(1, m.MinHeight), h)
	}
	m.state.NormalizeViewport(m.config())
}

// Normalize applies the allocated outer width and editable height without rendering.
func (m *Model) Normalize(width, rows int) { m.SetWidth(width); m.SetHeight(rows) }
func (m *Model) Width() int                { return m.width }
func (m *Model) SetWidth(width int) {
	if m.MaxWidth > 0 {
		width = min(width, m.MaxWidth)
	}
	oldWidth := m.width
	m.width = max(1, width-m.active().Base.GetHorizontalFrameSize()-ansi.StringWidth(m.Prompt)-m.gutterWidth())
	if oldWidth != m.width {
		m.generation++
	}
	m.normalize()
}
func (m *Model) Height() int { return m.height }
func (m *Model) SetHeight(height int) {
	if m.MaxContentHeight > 0 {
		height = min(height, m.MaxContentHeight)
	}
	oldHeight := m.height
	m.height = max(max(1, m.MinHeight), height)
	if oldHeight != m.height {
		m.generation++
	}
	m.state.NormalizeViewport(m.config())
}
func (m *Model) Value() string { return m.state.Value() }
func (m *Model) SetValue(value string) {
	value = normalizeNewlines(value)
	if m.MaxContentHeight > 0 {
		m.state.SetValue("")
		m.InsertString(value)
		m.normalize()
		return
	}
	m.state.SetCharLimit(m.CharLimit)
	if m.MaxHeight > 0 {
		lines := strings.Split(value, "\n")
		if len(lines) > m.MaxHeight {
			value = strings.Join(lines[:m.MaxHeight], "\n")
		}
	}
	m.state.SetValue(value)
	m.state.Apply(text.Action{Kind: text.MoveDocumentEnd})
	m.normalize()
}
func (m *Model) InsertString(value string) {
	value = normalizeNewlines(value)
	m.state.SetCharLimit(m.CharLimit)
	// A selection may free logical lines before the inserted text is limited.
	available := m.MaxHeight - m.state.LineCount() + strings.Count(m.state.SelectedText(), "\n")
	if m.MaxHeight > 0 && strings.Count(value, "\n") > max(0, available) {
		parts := strings.Split(value, "\n")
		value = strings.Join(parts[:max(0, available)+1], "\n")
	}
	if m.MaxContentHeight > 0 {
		for {
			candidate := m.state.Clone()
			candidate.Insert(value)
			if candidate.VisualLineCount(m.config()) <= m.MaxContentHeight {
				m.state = candidate
				break
			}
			i := strings.LastIndex(value, "\n")
			if i < 0 {
				return
			}
			value = value[:i]
		}
	} else {
		m.state.Insert(value)
	}
	m.normalize()
}
func normalizeNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func (m *Model) Reset()                { m.SetValue("") }
func (m *Model) Word() string          { return m.state.Word() }
func (m *Model) SelectedText() string  { return m.state.SelectedText() }
func (m *Model) Line() int             { return m.state.Line() }
func (m *Model) Column() int           { return m.state.Column() }
func (m *Model) LineCount() int        { return m.state.LineCount() }
func (m *Model) ScrollYOffset() int    { return m.state.ScrollYOffset() }
func (m *Model) LineInfo() LineInfo    { return m.state.LineInfo(m.config()) }
func (m *Model) Layout() text.Layout   { return m.state.Layout(m.config()) }
func (m *Model) ContentLineCount() int { return m.state.VisualLineCount(m.config()) }
func (m *Model) PositionAtCell(row, column int) text.Position {
	return m.state.PositionAtCell(m.config(), row, column)
}
func (m *Model) SetCursorPosition(p text.Position) { m.state.SetCursor(p); m.normalize() }
func (m *Model) SetCursorColumn(column int) {
	m.SetCursorPosition(text.Position{Line: m.Line(), Column: column})
}
func (m *Model) apply(kind text.ActionKind, selecting bool) {
	m.state.Apply(text.Action{Kind: kind, Select: selecting, Config: m.config()})
	m.normalize()
}
func (m *Model) MoveToBegin()  { m.apply(text.MoveDocumentStart, false) }
func (m *Model) MoveToEnd()    { m.apply(text.MoveDocumentEnd, false) }
func (m *Model) CursorUp()     { m.apply(text.MoveUp, false) }
func (m *Model) CursorDown()   { m.apply(text.MoveDown, false) }
func (m *Model) CursorStart()  { m.apply(text.MoveHome, false) }
func (m *Model) CursorEnd()    { m.apply(text.MoveEnd, false) }
func (m *Model) Focused() bool { return m.focused }
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

// Revision covers mutation methods. Owners assigning exported configuration fields
// directly must also invalidate their presentation cache.
func (m *Model) Revision() uint64        { return m.state.Revision() + m.generation }
func (m *Model) ContentRevision() uint64 { return m.state.ContentRevision() }
func (m *Model) Cursor() *tea.Cursor {
	if m.virtual || !m.focused {
		return nil
	}
	cell := m.state.CursorCell(m.config())
	s := m.active()
	c := tea.NewCursor(cell.Column+ansi.StringWidth(m.Prompt)+m.gutterWidth()+s.Base.GetPaddingLeft()+s.Base.GetBorderLeftSize()+s.Base.GetMarginLeft(), cell.Row-m.state.ScrollYOffset()+s.Base.GetPaddingTop()+s.Base.GetBorderTopSize()+s.Base.GetMarginTop())
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
func copySelection(value string) tea.Cmd { return func() tea.Msg { return clipboard.WriteAll(value) } }

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
		if key.Matches(v, m.KeyMap.CopySelection) {
			if selected := m.SelectedText(); selected != "" {
				return m, copySelection(selected)
			}
			return m, nil
		}
		if key.Matches(v, m.KeyMap.InsertNewline) {
			m.InsertString("\n")
			return m, m.blink.Reset(m.styles.Cursor)
		}
		if key.Matches(v, m.KeyMap.PageUp, m.KeyMap.PageDown) {
			kind := text.MoveUp
			if key.Matches(v, m.KeyMap.PageDown) {
				kind = text.MoveDown
			}
			for range max(1, m.height) {
				m.apply(kind, false)
			}
			return m, m.blink.Reset(m.styles.Cursor)
		}
		if key.Matches(v, m.KeyMap.CapitalizeWordForward, m.KeyMap.LowercaseWordForward, m.KeyMap.UppercaseWordForward) {
			m.changeWord(v)
			return m, m.blink.Reset(m.styles.Cursor)
		}
		if key.Matches(v, m.KeyMap.TransposeCharacterBackward) {
			m.transpose()
			return m, m.blink.Reset(m.styles.Cursor)
		}
		actions := []struct {
			binding   key.Binding
			kind      text.ActionKind
			selecting bool
		}{
			{m.KeyMap.CharacterForward, text.MoveRight, false}, {m.KeyMap.CharacterBackward, text.MoveLeft, false},
			{m.KeyMap.WordForward, text.MoveWordRight, false}, {m.KeyMap.WordBackward, text.MoveWordLeft, false},
			{m.KeyMap.LineNext, text.MoveDown, false}, {m.KeyMap.LinePrevious, text.MoveUp, false},
			{m.KeyMap.LineStart, text.MoveHome, false}, {m.KeyMap.LineEnd, text.MoveEnd, false},
			{m.KeyMap.InputBegin, text.MoveDocumentStart, false}, {m.KeyMap.InputEnd, text.MoveDocumentEnd, false},
			{m.KeyMap.DeleteCharacterBackward, text.DeleteBackward, false}, {m.KeyMap.DeleteCharacterForward, text.DeleteForward, false},
			{m.KeyMap.DeleteWordBackward, text.DeleteWordBackward, false}, {m.KeyMap.DeleteWordForward, text.DeleteWordForward, false},
			{m.KeyMap.DeleteBeforeCursor, text.DeleteToStart, false}, {m.KeyMap.DeleteAfterCursor, text.DeleteToEnd, false},
			{m.KeyMap.SelectCharacterForward, text.MoveRight, true}, {m.KeyMap.SelectCharacterBackward, text.MoveLeft, true},
			{m.KeyMap.SelectWordForward, text.MoveWordRight, true}, {m.KeyMap.SelectWordBackward, text.MoveWordLeft, true},
			{m.KeyMap.SelectLineUp, text.MoveUp, true}, {m.KeyMap.SelectLineDown, text.MoveDown, true},
			{m.KeyMap.SelectAll, text.SelectEverything, true},
		}
		for _, a := range actions {
			if key.Matches(v, a.binding) {
				m.apply(a.kind, a.selecting)
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

func (m *Model) changeWord(msg tea.KeyPressMsg) {
	start := m.state.Cursor()
	m.state.Apply(text.Action{Kind: text.MoveWordRight, Select: true, Config: m.config()})
	value := m.state.SelectedText()
	switch {
	case key.Matches(msg, m.KeyMap.LowercaseWordForward):
		value = strings.ToLower(value)
	case key.Matches(msg, m.KeyMap.UppercaseWordForward):
		value = strings.ToUpper(value)
	default:
		first := true
		value = strings.Map(func(r rune) rune {
			if !unicode.IsSpace(r) && first {
				first = false
				return unicode.ToTitle(r)
			}
			return r
		}, value)
	}
	if value == "" {
		m.state.SetCursor(start)
	} else {
		m.state.Insert(value)
	}
	m.normalize()
}
func (m *Model) transpose() {
	// Select two complete graphemes on the current logical line.
	line := strings.Split(m.Value(), "\n")[m.Line()]
	g := uniseg.NewGraphemes(line)
	var parts []string
	var ends []int
	column := 0
	for g.Next() {
		parts = append(parts, g.Str())
		column += len([]rune(g.Str()))
		ends = append(ends, column)
	}
	if len(parts) < 2 || m.Column() == 0 {
		return
	}
	right := len(parts) - 1
	for i, end := range ends {
		if end > m.Column() {
			right = i
			break
		}
	}
	left := right - 1
	if left < 0 {
		return
	}
	begin := 0
	if left > 0 {
		begin = ends[left-1]
	}
	m.state.Select(text.Position{Line: m.Line(), Column: begin}, text.Position{Line: m.Line(), Column: ends[right]})
	m.state.Insert(parts[right] + parts[left])
	m.normalize()
}

// View is a read-only projection. All cursor-following and reflow occurs in mutations.
func (m Model) View() string {
	l := m.Layout()
	s := m.active()
	placeholder := m.Value() == "" && m.Placeholder != ""
	if placeholder {
		preview := text.New(text.Options{Multiline: true})
		preview.SetValue(m.Placeholder)
		l.Rows = preview.Layout(m.config()).Rows
	}
	lines := make([]string, m.height)
	for y := range m.height {
		rowIndex := y + l.ScrollY
		prefix := s.Prompt.Render(m.Prompt)
		if m.ShowLineNumbers {
			number := ""
			if rowIndex < len(l.Rows) && l.Rows[rowIndex].StartColumn == 0 {
				number = strconv.Itoa(l.Rows[rowIndex].Line + 1)
			}
			style := s.LineNumber
			if rowIndex < len(l.Rows) && l.Rows[rowIndex].Line == m.Line() {
				style = s.CursorLineNumber
			}
			prefix += style.Render(fmt.Sprintf("%*s  ", m.gutterWidth()-2, number))
		}
		lineStyle := s.Text.Inherit(s.Base).Inline(true)
		if rowIndex < len(l.Rows) && l.Rows[rowIndex].Line == m.Line() {
			lineStyle = s.CursorLine.Inherit(s.Text).Inherit(s.Base).Inline(true)
		}
		if rowIndex >= len(l.Rows) {
			lineStyle = s.EndOfBuffer.Inherit(s.Base).Inline(true)
		}
		contentStyle := lineStyle
		if placeholder {
			contentStyle = s.Placeholder.Inherit(s.Base).Inline(true)
		}
		selectionStyle := s.Selection.Inherit(s.Text).Inherit(s.Base).Inline(true)
		var body, span strings.Builder
		cells := 0
		selected := false
		visible := m.focused && m.virtual && m.blink.Visible(m.styles.Cursor) && rowIndex == l.Cursor.Row
		// Each style transition owns a complete span, including a reset. Never
		// enclose a cursor/selection reset in one outer text span: that would
		// silently discard the text foreground following the decorated cell.
		flush := func() {
			if span.Len() == 0 {
				return
			}
			style := contentStyle
			if selected {
				style = selectionStyle
			}
			body.WriteString(style.Render(span.String()))
			span.Reset()
		}
		if rowIndex < len(l.Rows) {
			for _, run := range l.Rows[rowIndex].Runs {
				isSelected := run.Selected && !placeholder
				if isSelected != selected {
					flush()
					selected = isSelected
				}
				if visible && run.Cell == l.Cursor.Column && !isSelected {
					flush()
					body.WriteString(contentStyle.Render(cursor.Render(run.Text, m.styles.Cursor)))
				} else {
					span.WriteString(run.Text)
				}
				cells += run.Width
			}
		}
		flush()
		// A selected newline occupies the cell immediately after the text.
		newlineSelected := false
		for _, selection := range l.Selection {
			if selection.Row == rowIndex && selection.End > cells {
				newlineSelected = true
				break
			}
		}
		if cells < m.width && newlineSelected && !placeholder {
			body.WriteString(selectionStyle.Render(" "))
			cells++
		}
		if cells < m.width {
			if visible && l.Cursor.Column >= cells && l.Cursor.Column < m.width && !newlineSelected {
				body.WriteString(lineStyle.Render(strings.Repeat(" ", l.Cursor.Column-cells)))
				body.WriteString(lineStyle.Render(cursor.Render(" ", m.styles.Cursor)))
				cells = l.Cursor.Column + 1
			}
			body.WriteString(lineStyle.Render(strings.Repeat(" ", m.width-cells)))
		}
		lines[y] = prefix + ansi.Truncate(body.String(), m.width, "")
	}
	return s.Base.Render(strings.Join(lines, "\n"))
}
