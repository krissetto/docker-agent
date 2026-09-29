// Package retained provides one-slot retention infrastructure for string renderers.
// It owns no UI state and has no terminal or framework dependencies.
package retained

// Slot retains only the last input and output. The zero value is ready to use.
// Inputs must contain every value draw consumes; draw must not consult mutable
// owner state. Use one stable drawing function per slot. Slots are not concurrent.
type Slot[I comparable] struct {
	input   I
	output  string
	valid   bool
	renders uint64
}

// Render draws only when the complete input differs from the retained input.
func (s *Slot[I]) Render(in I, draw func(I) string) string {
	if s.valid && s.input == in {
		return s.output
	}
	s.output = s.Fresh(in, draw)
	s.input, s.valid = in, true
	return s.output
}

// Fresh draws unconditionally without replacing the retained input or output.
func (s *Slot[I]) Fresh(in I, draw func(I) string) string {
	s.renders++
	return draw(in)
}

// Renders counts actual draws, including forced-fresh draws, not cache hits.
func (s *Slot[I]) Renders() uint64 { return s.renders }
