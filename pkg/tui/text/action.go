package text

import "unicode"

type ActionKind uint8

const (
	InsertText ActionKind = iota
	DeleteBackward
	DeleteForward
	InsertNewline
	MoveLeft
	MoveRight
	MoveWordLeft
	MoveWordRight
	MoveHome
	MoveEnd
	MoveDocumentStart
	MoveDocumentEnd
	MoveUp
	MoveDown
	DeleteWordBackward
	DeleteWordForward
	DeleteToStart
	DeleteToEnd
	SelectEverything
)

// Action contains no terminal event, focus, style, or key binding. Select
// extends a movement from its original anchor. Config is used by vertical motion.
type Action struct {
	Kind   ActionKind
	Text   string
	Select bool
	Config Config
}

func (s State) spaceAt(p Position) bool {
	if p.Column == s.line(p.Line).runes {
		return true
	}
	for _, g := range s.line(p.Line).clusters {
		if g.start == p.Column {
			return whitespace(g)
		}
	}
	return true
}
func (s State) wordLeft(p Position) Position {
	for {
		previous := s.previous(p)
		if previous == p || !s.spaceAt(previous) {
			break
		}
		p = previous
	}
	for {
		previous := s.previous(p)
		if previous == p || s.spaceAt(previous) {
			break
		}
		p = previous
	}
	return p
}
func (s State) wordRight(p Position) Position {
	for {
		next := s.next(p)
		if next == p || s.spaceAt(p) {
			break
		}
		p = next
	}
	for {
		next := s.next(p)
		if next == p || !s.spaceAt(p) {
			break
		}
		p = next
	}
	return p
}

// Word returns the whitespace-delimited word containing (or immediately before)
// the insertion position. It does not change the cursor or selection.
func (s State) Word() string {
	value := []rune(s.line(s.cursor.Line).text)
	start, end := s.cursor.Column, s.cursor.Column
	for start > 0 && !unicode.IsSpace(value[start-1]) {
		start--
	}
	for end < len(value) && !unicode.IsSpace(value[end]) {
		end++
	}
	return string(value[start:end])
}
func (s *State) vertical(delta int, selecting bool, config Config) {
	c := normalizedConfig(config)
	rows := s.rows(c)
	cursor := rowCursor(rows, s.cursor)
	if !s.hasPreferredX {
		s.preferredX = cursor.Column
		s.hasPreferredX = true
	}
	target := max(0, min(cursor.Row+delta, len(rows)-1))
	p := positionInRow(rows[target], s.preferredX)
	// At a soft-wrap seam the same rune position belongs to the following row.
	// Stay on the requested visual row by choosing its last grapheme's start.
	if target+1 < len(rows) && rows[target+1].Line == p.Line && rows[target+1].StartColumn == p.Column && len(rows[target].Runs) > 0 {
		p.Column = rows[target].Runs[len(rows[target].Runs)-1].StartColumn
	}
	s.move(p, selecting, true)
}
func (s *State) Apply(action Action) {
	p := s.cursor
	switch action.Kind {
	case InsertText:
		s.Insert(action.Text)
		return
	case DeleteBackward:
		s.Backspace()
		return
	case DeleteForward:
		s.Delete()
		return
	case InsertNewline:
		s.Newline()
		return
	case SelectEverything:
		s.SelectAll()
		return
	case MoveLeft:
		if a, _, ok := s.selection(); ok && !action.Select {
			p = a
		} else {
			p = s.previous(p)
		}
	case MoveRight:
		if _, b, ok := s.selection(); ok && !action.Select {
			p = b
		} else {
			p = s.next(p)
		}
	case MoveWordLeft:
		p = s.wordLeft(p)
	case MoveWordRight:
		p = s.wordRight(p)
	case MoveHome:
		p.Column = 0
	case MoveEnd:
		p.Column = s.line(p.Line).runes
	case MoveDocumentStart:
		p = Position{}
	case MoveDocumentEnd:
		p = Position{s.LineCount() - 1, s.line(s.LineCount() - 1).runes}
	case MoveUp:
		s.vertical(-1, action.Select, action.Config)
		return
	case MoveDown:
		s.vertical(1, action.Select, action.Config)
		return
	case DeleteWordBackward:
		a, b, ok := s.selection()
		if !ok {
			a = s.wordLeft(p)
		}
		s.replace(a, b, "")
		return
	case DeleteWordForward:
		a, b, ok := s.selection()
		if !ok {
			b = s.wordRight(p)
		}
		s.replace(a, b, "")
		return
	case DeleteToStart:
		a, b, ok := s.selection()
		if !ok {
			a = Position{p.Line, 0}
		}
		s.replace(a, b, "")
		return
	case DeleteToEnd:
		a, b, ok := s.selection()
		if !ok {
			b = Position{p.Line, s.line(p.Line).runes}
		}
		s.replace(a, b, "")
		return
	default:
		return
	}
	s.move(p, action.Select, false)
}
