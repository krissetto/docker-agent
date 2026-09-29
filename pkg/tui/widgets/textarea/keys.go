package textarea

import "github.com/docker/docker-agent/pkg/tui/widgets/key"

// KeyMap defines configurable editing bindings.
type KeyMap struct {
	CharacterForward           key.Binding
	CharacterBackward          key.Binding
	WordForward                key.Binding
	WordBackward               key.Binding
	DeleteWordBackward         key.Binding
	DeleteWordForward          key.Binding
	DeleteAfterCursor          key.Binding
	DeleteBeforeCursor         key.Binding
	DeleteCharacterBackward    key.Binding
	DeleteCharacterForward     key.Binding
	LineStart                  key.Binding
	LineEnd                    key.Binding
	Paste                      key.Binding
	InsertNewline              key.Binding
	LineNext                   key.Binding
	LinePrevious               key.Binding
	PageUp                     key.Binding
	PageDown                   key.Binding
	InputBegin                 key.Binding
	InputEnd                   key.Binding
	CapitalizeWordForward      key.Binding
	LowercaseWordForward       key.Binding
	UppercaseWordForward       key.Binding
	TransposeCharacterBackward key.Binding
	SelectCharacterForward     key.Binding
	SelectCharacterBackward    key.Binding
	SelectWordForward          key.Binding
	SelectWordBackward         key.Binding
	SelectLineUp               key.Binding
	SelectLineDown             key.Binding
	SelectAll                  key.Binding
	CopySelection              key.Binding
}

func DefaultKeyMap() KeyMap {
	return KeyMap{
		CharacterForward:           key.NewBinding(key.WithKeys("right", "ctrl+f"), key.WithHelp("right", "character forward")),
		CharacterBackward:          key.NewBinding(key.WithKeys("left", "ctrl+b"), key.WithHelp("left", "character backward")),
		WordForward:                key.NewBinding(key.WithKeys("alt+right", "ctrl+right", "alt+f"), key.WithHelp("alt+right", "word forward")),
		WordBackward:               key.NewBinding(key.WithKeys("alt+left", "ctrl+left", "alt+b"), key.WithHelp("alt+left", "word backward")),
		DeleteWordBackward:         key.NewBinding(key.WithKeys("alt+backspace", "ctrl+w", "ctrl+backspace"), key.WithHelp("alt+backspace", "delete word backward")),
		DeleteWordForward:          key.NewBinding(key.WithKeys("alt+delete", "alt+d", "ctrl+delete"), key.WithHelp("alt+delete", "delete word forward")),
		DeleteAfterCursor:          key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("ctrl+k", "delete after cursor")),
		DeleteBeforeCursor:         key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl+u", "delete before cursor")),
		DeleteCharacterBackward:    key.NewBinding(key.WithKeys("backspace", "ctrl+h"), key.WithHelp("backspace", "delete character backward")),
		DeleteCharacterForward:     key.NewBinding(key.WithKeys("delete", "ctrl+d"), key.WithHelp("delete", "delete character forward")),
		LineStart:                  key.NewBinding(key.WithKeys("home", "ctrl+a"), key.WithHelp("home", "line start")),
		LineEnd:                    key.NewBinding(key.WithKeys("end", "ctrl+e"), key.WithHelp("end", "line end")),
		Paste:                      key.NewBinding(key.WithKeys("ctrl+v"), key.WithHelp("ctrl+v", "paste")),
		InsertNewline:              key.NewBinding(key.WithKeys("enter", "ctrl+m"), key.WithHelp("enter", "insert newline")),
		LineNext:                   key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("down", "next line")),
		LinePrevious:               key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("up", "previous line")),
		PageUp:                     key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up")),
		PageDown:                   key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdown", "page down")),
		InputBegin:                 key.NewBinding(key.WithKeys("alt+<", "ctrl+home"), key.WithHelp("alt+<", "input begin")),
		InputEnd:                   key.NewBinding(key.WithKeys("alt+>", "ctrl+end"), key.WithHelp("alt+>", "input end")),
		CapitalizeWordForward:      key.NewBinding(key.WithKeys("alt+c"), key.WithHelp("alt+c", "capitalize word forward")),
		LowercaseWordForward:       key.NewBinding(key.WithKeys("alt+l"), key.WithHelp("alt+l", "lowercase word forward")),
		UppercaseWordForward:       key.NewBinding(key.WithKeys("alt+u"), key.WithHelp("alt+u", "uppercase word forward")),
		TransposeCharacterBackward: key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "transpose character backward")),
		SelectCharacterForward:     key.NewBinding(key.WithKeys("shift+right"), key.WithHelp("shift+right", "select character forward")),
		SelectCharacterBackward:    key.NewBinding(key.WithKeys("shift+left"), key.WithHelp("shift+left", "select character backward")),
		SelectWordForward:          key.NewBinding(key.WithKeys("ctrl+shift+right", "alt+shift+right", "alt+shift+f"), key.WithHelp("ctrl+shift+right", "select word forward")),
		SelectWordBackward:         key.NewBinding(key.WithKeys("ctrl+shift+left", "alt+shift+left", "alt+shift+b"), key.WithHelp("ctrl+shift+left", "select word backward")),
		SelectLineUp:               key.NewBinding(key.WithKeys("shift+up"), key.WithHelp("shift+up", "select line up")),
		SelectLineDown:             key.NewBinding(key.WithKeys("shift+down"), key.WithHelp("shift+down", "select line down")),
		SelectAll:                  key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("ctrl+g", "select all")),
		CopySelection:              key.NewBinding(key.WithKeys("ctrl+shift+c"), key.WithHelp("ctrl+shift+c", "copy selection")),
	}
}
