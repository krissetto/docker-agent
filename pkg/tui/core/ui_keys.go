package core

import "github.com/docker/docker-agent/pkg/tui/widgets/key"

// UIKeys are the shared navigation and confirmation keys for list-like
// widgets. They are fixed, not remappable: user remaps apply only to KeyMap.
// Text-input contexts should omit the Vim runes so typing is not intercepted.
var UIKeys = struct {
	Up, Down, Left, Right, PageUp, PageDown, Home, End,
	Confirm, Toggle, Cancel, NextField, PrevField,
	VimUp, VimDown, VimLeft, VimRight, VimTop, VimBottom,
	ListUp, ListDown, Quit key.Binding
}{
	Up:        key.NewBinding(key.WithKeys("up")),
	Down:      key.NewBinding(key.WithKeys("down")),
	Left:      key.NewBinding(key.WithKeys("left")),
	Right:     key.NewBinding(key.WithKeys("right")),
	PageUp:    key.NewBinding(key.WithKeys("pgup")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown")),
	Home:      key.NewBinding(key.WithKeys("home")),
	End:       key.NewBinding(key.WithKeys("end")),
	Confirm:   key.NewBinding(key.WithKeys("enter")),
	Toggle:    key.NewBinding(key.WithKeys("space")),
	Cancel:    key.NewBinding(key.WithKeys("esc")),
	NextField: key.NewBinding(key.WithKeys("tab")),
	PrevField: key.NewBinding(key.WithKeys("shift+tab")),
	VimUp:     key.NewBinding(key.WithKeys("k")),
	VimDown:   key.NewBinding(key.WithKeys("j")),
	VimLeft:   key.NewBinding(key.WithKeys("h")),
	VimRight:  key.NewBinding(key.WithKeys("l")),
	VimTop:    key.NewBinding(key.WithKeys("g")),
	VimBottom: key.NewBinding(key.WithKeys("G")),
	ListUp:    key.NewBinding(key.WithKeys("ctrl+k")),
	ListDown:  key.NewBinding(key.WithKeys("ctrl+j")),
	Quit:      key.NewBinding(key.WithKeys("q")),
}
