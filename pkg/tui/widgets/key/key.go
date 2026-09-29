// Package key defines application key bindings without depending on a terminal
// event implementation. Events are matched by their string representation.
package key

import "slices"

// Help labels a binding for display; Key need not be one of its input keys.
type Help struct {
	Key  string
	Desc string
}

// Binding associates input keys with help text and an enabled state. Copies can
// be changed independently; key slices passed in or returned are detached.
type Binding struct {
	keys     []string
	help     Help
	disabled bool
}

// BindingOpt configures a newly constructed binding.
type BindingOpt func(*Binding)

// NewBinding applies options in order to an initially unbound binding.
func NewBinding(options ...BindingOpt) Binding {
	var binding Binding
	for _, option := range options {
		option(&binding)
	}
	return binding
}

// WithKeys captures the input keys for a binding.
func WithKeys(keys ...string) BindingOpt {
	keys = slices.Clone(keys)
	return func(binding *Binding) {
		binding.SetKeys(keys...)
	}
}

// WithHelp supplies the display label and description.
func WithHelp(label, description string) BindingOpt {
	return func(binding *Binding) {
		binding.SetHelp(label, description)
	}
}

// Keys returns a detached copy of the input keys.
func (b Binding) Keys() []string { return slices.Clone(b.keys) }

// SetKeys replaces the input keys without retaining the caller's slice.
func (b *Binding) SetKeys(keys ...string) { b.keys = slices.Clone(keys) }

// Help returns the display label and description.
func (b Binding) Help() Help { return b.help }

// SetHelp replaces the display label and description.
func (b *Binding) SetHelp(label, description string) {
	b.help = Help{Key: label, Desc: description}
}

// Enabled reports whether a binding has keys and has not been disabled. A nil
// key slice is unbound; an explicitly empty slice is enabled but matches nothing.
func (b Binding) Enabled() bool { return b.keys != nil && !b.disabled }

// SetEnabled toggles a binding without changing its keys or help.
func (b *Binding) SetEnabled(enabled bool) { b.disabled = !enabled }

// Matches reports whether the event's exact string is an enabled input key of
// any binding. Display labels are not input aliases.
func Matches[Event interface{ String() string }](event Event, bindings ...Binding) bool {
	input := event.String()
	for _, binding := range bindings {
		if binding.Enabled() && slices.Contains(binding.keys, input) {
			return true
		}
	}
	return false
}
