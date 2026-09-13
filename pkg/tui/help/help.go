// Package help contains presentation-only keyboard help snapshots.
// Entries describe existing behavior; they do not register or dispatch actions.
package help

// Entry groups aliases for one action in one context. Keys retain canonical spelling.
type Entry struct {
	ID          string
	Keys        []string
	Description string
	Condition   string
}

// Section groups related actions under a stable semantic identity.
type Section struct {
	ID      string
	Title   string
	Entries []Entry
}

// Document separates the captured current context from other-context reference.
type Document struct {
	Context   string
	Current   []Section
	Reference []Section
}
