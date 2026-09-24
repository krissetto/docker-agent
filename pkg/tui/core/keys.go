package core

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"charm.land/bubbles/v2/key"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// KeyMap contains the keybindings used across the TUI. Bindings are resolved
// once from DefaultKeyMap() merged with the user's overrides (see GetKeys).
type KeyMap struct {
	SessionsBrowser                                                                  key.Binding
	WorkspaceNext, WorkspacePrevious, WorkspaceCreate, WorkspaceClose, WorkspaceMove key.Binding
	WorkspaceDirect                                                                  [9]key.Binding
	Quit                                                                             key.Binding
	SwitchFocus                                                                      key.Binding
	Commands                                                                         key.Binding
	Help                                                                             key.Binding
	ToggleYolo                                                                       key.Binding
	ToggleHideToolResults                                                            key.Binding
	CycleAgent                                                                       key.Binding
	ModelPicker                                                                      key.Binding
	Suspend                                                                          key.Binding
	ToggleSidebar                                                                    key.Binding
	EditExternal                                                                     key.Binding
	HistorySearch                                                                    key.Binding

	// EditorSend submits the editor content. EditorNewline inserts a line
	// break. These are the input bindings referenced by issue #1626. The
	// editor additionally offers shift+enter for newline on terminals that
	// can report it; that is detected at runtime and is not part of the
	// configurable default.
	EditorSend    key.Binding
	EditorNewline key.Binding
}

var (
	ckMutex    sync.RWMutex
	cachedKeys *KeyMap
)

// keyModifiers are the modifier prefixes Bubbletea emits, in any order.
var keyModifiers = map[string]bool{
	"ctrl": true, "alt": true, "shift": true,
	"meta": true, "hyper": true, "super": true,
}

// namedKeys are the multi-character key names Bubbletea can emit (function
// keys fN are handled separately). Validating against this set turns a typo
// like "entre" or "ctrl+foobar" into an ignored-with-warning entry instead of
// a silently dead binding that could lock the user out of an action.
var namedKeys = map[string]bool{
	"backspace": true, "begin": true, "capslock": true, "comma": true,
	"delete": true, "div": true, "down": true, "end": true, "enter": true,
	"equal": true, "esc": true, "find": true, "home": true, "insert": true,
	"left": true, "menu": true, "minus": true, "mul": true, "mute": true,
	"numlock": true, "pause": true, "period": true, "pgdown": true,
	"pgup": true, "plus": true, "printscreen": true, "right": true,
	"scrolllock": true, "select": true, "sep": true, "space": true,
	"tab": true, "up": true,
}

// reservedKeys are bound by the TUI outside the configurable KeyMap (tab bar,
// agent quick-switch, thinking-level, cancel). Seeding them into the conflict
// table makes an override onto one of them rejected-with-warning rather than
// silently shadowed by the built-in handler.
var reservedKeys = []string{
	"ctrl+t", "ctrl+w", "ctrl+p", "ctrl+n", // tab bar
	"shift+tab", // cycle thinking level
	"alt+enter", // end-of-turn follow-up
	"esc",       // cancel / dismiss
	"ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5",
	"ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9", // agent quick-switch
}

// DefaultKeyMap returns the built-in keybindings used when the user has not
// overridden them. Newline defaults to ctrl+j only; shift+enter is layered on
// at runtime by the editor when the terminal supports it.
func DefaultKeyMap() KeyMap {
	keys := KeyMap{
		Quit:                  key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("Ctrl+c", "quit")),
		SwitchFocus:           key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "switch focus")),
		Commands:              key.NewBinding(key.WithKeys("ctrl+k"), key.WithHelp("Ctrl+k", "commands")),
		Help:                  key.NewBinding(key.WithKeys("ctrl+h", "f1", "ctrl+?"), key.WithHelp("Ctrl+h", "help")),
		ToggleYolo:            key.NewBinding(key.WithKeys("ctrl+y"), key.WithHelp("Ctrl+y", "toggle yolo mode")),
		ToggleHideToolResults: key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("Ctrl+o", "toggle hide tool results")),
		CycleAgent:            key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("Ctrl+s", "cycle agent")),
		ModelPicker:           key.NewBinding(key.WithKeys("ctrl+m"), key.WithHelp("Ctrl+m", "model picker")),
		Suspend:               key.NewBinding(key.WithKeys("ctrl+z"), key.WithHelp("Ctrl+z", "suspend")),
		ToggleSidebar:         key.NewBinding(key.WithKeys("ctrl+b"), key.WithHelp("Ctrl+b", "toggle sidebar")),
		EditExternal:          key.NewBinding(key.WithKeys("ctrl+g"), key.WithHelp("Ctrl+g", "edit in external editor")),
		HistorySearch:         key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("Ctrl+r", "history search")),
		EditorSend:            key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "send message")),
		EditorNewline:         key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("Ctrl+j", "insert newline")),
	}
	keys.SessionsBrowser = key.NewBinding(key.WithKeys("alt+b"), key.WithHelp("Alt+b", "sessions browser"))
	keys.WorkspaceNext = key.NewBinding(key.WithKeys("alt+]"), key.WithHelp("Alt+]", "next workspace"))
	keys.WorkspacePrevious = key.NewBinding(key.WithKeys("alt+["), key.WithHelp("Alt+[", "previous workspace"))
	keys.WorkspaceCreate = key.NewBinding(key.WithKeys("alt+n"), key.WithHelp("Alt+n", "new workspace"))
	keys.WorkspaceClose = key.NewBinding(key.WithKeys("alt+w"), key.WithHelp("Alt+w", "close workspace"))
	keys.WorkspaceMove = key.NewBinding(key.WithKeys("alt+m"), key.WithHelp("Alt+m", "move pane to next workspace"))
	for i := range keys.WorkspaceDirect {
		combo := fmt.Sprintf("alt+%d", i+1)
		keys.WorkspaceDirect[i] = key.NewBinding(key.WithKeys(combo), key.WithHelp(combo, fmt.Sprintf("workspace %d", i+1)))
	}
	return keys
}

// actionEntry links an action name to its binding pointer (inside a working
// KeyMap) and the human-readable description kept across remaps.
type actionEntry struct {
	action  string
	binding *key.Binding
	help    string
}

// actionMapFor lists the remappable actions for a working KeyMap. The slice
// order also defines conflict-resolution priority: earlier actions keep a
// contested key, later ones lose it.
func actionMapFor(keys *KeyMap) []actionEntry {
	entries := []actionEntry{
		{"quit", &keys.Quit, "quit"},
		{"switch_focus", &keys.SwitchFocus, "switch focus"},
		{"commands", &keys.Commands, "commands"},
		{"help", &keys.Help, "help"},
		{"toggle_yolo", &keys.ToggleYolo, "toggle yolo mode"},
		{"toggle_hide_tool_results", &keys.ToggleHideToolResults, "toggle hide tool results"},
		{"cycle_agent", &keys.CycleAgent, "cycle agent"},
		{"model_picker", &keys.ModelPicker, "model picker"},
		{"suspend", &keys.Suspend, "suspend"},
		{"toggle_sidebar", &keys.ToggleSidebar, "toggle sidebar"},
		{"edit_external", &keys.EditExternal, "edit in external editor"},
		{"history_search", &keys.HistorySearch, "history search"},
		{"editor_send", &keys.EditorSend, "send message"},
		{"editor_newline", &keys.EditorNewline, "insert newline"},
	}
	entries = append(entries, actionEntry{"sessions_browser", &keys.SessionsBrowser, "sessions browser"}, actionEntry{"workspace_next", &keys.WorkspaceNext, "next workspace"}, actionEntry{"workspace_previous", &keys.WorkspacePrevious, "previous workspace"}, actionEntry{"workspace_create", &keys.WorkspaceCreate, "new workspace"}, actionEntry{"workspace_close", &keys.WorkspaceClose, "close workspace"}, actionEntry{"workspace_move", &keys.WorkspaceMove, "move pane to next workspace"})
	for i := range keys.WorkspaceDirect {
		entries = append(entries, actionEntry{fmt.Sprintf("workspace_%d", i+1), &keys.WorkspaceDirect[i], fmt.Sprintf("workspace %d", i+1)})
	}
	return entries
}

// ValidActions returns the sorted list of action names that can be remapped.
// Useful for documentation and a future "show active bindings" command.
func ValidActions() []string {
	entries := actionMapFor(&KeyMap{})
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.action)
	}
	sort.Strings(names)
	return names
}

// isFunctionKey reports whether s is a function-key name f1..f63.
func isFunctionKey(s string) bool {
	if len(s) < 2 || s[0] != 'f' {
		return false
	}
	n := 0
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 63
}

// validateKey reports whether a key string can actually be produced by a key
// event, so that a typo (e.g. "entre", "ctrl+foobar") is rejected instead of
// silently replacing an action's working keys. A key is a chain of modifiers
// joined by "+" ending in a base key that is a single rune, a function key, or
// a known named key. This is intentionally strict: rejected keys fall back to
// the action's default with a logged warning, so a bad config never strands
// the user without a way to send, quit, or insert a newline.
func validateKey(k string) bool {
	if k == "" || strings.ContainsAny(k, " \t") {
		return false
	}
	parts := strings.Split(k, "+")
	for _, m := range parts[:len(parts)-1] {
		if !keyModifiers[m] {
			return false
		}
	}
	base := parts[len(parts)-1]
	if base == "" {
		return false
	}
	if len([]rune(base)) == 1 {
		return true
	}
	return isFunctionKey(base) || namedKeys[base]
}

// displayKey returns the help-label form of a key string, matching the TUI's
// convention of capitalizing the leading modifier (e.g. "ctrl+q" -> "Ctrl+q").
func displayKey(k string) string {
	if k == "" {
		return k
	}
	r := []rune(k)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// applyUserKeybindings overrides defaults in-place from the user config.
// boundKeys is seeded with the defaults of every action the user is NOT
// remapping, so a custom key that collides with another action's default is
// detected and skipped (not bound to two actions). Intra-config collisions are
// resolved by config order. Every rejection is logged.
func applyUserKeybindings(bindings []userconfig.Keybinding, entries []actionEntry) {
	byAction := make(map[string]actionEntry, len(entries))
	for _, e := range entries {
		byAction[e.action] = e
	}

	overridden := make(map[string]bool)
	for _, b := range bindings {
		if _, ok := byAction[b.Action]; ok {
			overridden[b.Action] = true
		}
	}

	boundKeys := make(map[string]string)
	boundKeys["f1"] = "help"
	for _, k := range reservedKeys {
		boundKeys[k] = "a built-in shortcut"
	}
	for _, e := range entries {
		if overridden[e.action] {
			continue
		}
		for _, k := range e.binding.Keys() {
			boundKeys[k] = e.action
		}
	}

	for _, b := range bindings {
		e, ok := byAction[b.Action]
		if !ok {
			slog.Warn("Ignoring unrecognized keybinding action",
				"action", b.Action, "valid_actions", strings.Join(ValidActions(), ", "))
			continue
		}
		if len(b.Keys) == 0 {
			slog.Warn("Ignoring keybinding with no keys", "action", b.Action)
			continue
		}

		var validKeys []string
		for _, raw := range b.Keys {
			k := strings.TrimSpace(raw)
			if !validateKey(k) {
				slog.Warn("Ignoring malformed key", "action", b.Action, "key", raw)
				continue
			}
			if owner, exists := boundKeys[k]; exists && owner != b.Action {
				slog.Warn("Ignoring conflicting key", "key", k, "action", b.Action, "conflicts_with", owner)
				continue
			}
			boundKeys[k] = b.Action
			validKeys = append(validKeys, k)
		}

		if len(validKeys) > 0 {
			*e.binding = key.NewBinding(key.WithKeys(validKeys...), key.WithHelp(displayKey(validKeys[0]), e.help))
		}
	}
}

// buildKeys merges user config overrides onto the defaults. It is split from
// GetKeys so it can be unit-tested with arbitrary settings.
func buildKeys(settings *userconfig.Settings) KeyMap {
	keys := DefaultKeyMap()
	if settings != nil && len(settings.Keybindings) > 0 {
		applyUserKeybindings(settings.Keybindings, actionMapFor(&keys))
	}
	if !slices.Contains(keys.Help.Keys(), "f1") {
		keys.Help.SetKeys(append(slices.Clone(keys.Help.Keys()), "f1")...)
	}
	return keys
}

// EditorNewlineKeys resolves the configured newline aliases and the optional
// enhanced-terminal fallback without shadowing any configured action.
func EditorNewlineKeys(enhanced bool) []string {
	keys := GetKeys()
	return editorNewlineKeys(keys, enhanced)
}

func editorNewlineKeys(keys KeyMap, enhanced bool) []string {
	aliases := slices.Clone(keys.EditorNewline.Keys())
	if !enhanced || slices.Contains(aliases, "shift+enter") {
		return aliases
	}
	for _, entry := range actionMapFor(&keys) {
		if entry.binding.Enabled() && slices.Contains(entry.binding.Keys(), "shift+enter") {
			return aliases
		}
	}
	return append([]string{"shift+enter"}, aliases...)
}

// GetKeys returns the resolved keybindings, merging the user config over the
// defaults. The result is read from disk once and cached; subsequent calls are
// lock-cheap so hot paths (key handling, help rendering) stay allocation-free.
func GetKeys() KeyMap {
	ckMutex.RLock()
	k := cachedKeys
	ckMutex.RUnlock()
	if k != nil {
		return *k
	}

	ckMutex.Lock()
	defer ckMutex.Unlock()
	if cachedKeys == nil {
		built := buildKeys(userconfig.Get())
		cachedKeys = &built
	}
	return *cachedKeys
}

// ResetKeys clears the cached keybindings so the next GetKeys call rebuilds
// them. Used by tests and available for a future hot-reload.
func ResetKeys() {
	ckMutex.Lock()
	defer ckMutex.Unlock()
	cachedKeys = nil
}
