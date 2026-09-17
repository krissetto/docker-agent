package messages

// OpenPanesMsg opens the full-TUI layout action picker.
type OpenPanesMsg struct {
	Arguments string
}

// PaneActionMsg addresses canonical routing IDs, independent of tab order.
// Action is split, focus, remove, single, sources, or resize.
// Edge is left, right, up, or down for split/source selection.
type PaneActionMsg struct {
	Action    string
	Source    string
	Target    string
	Edge      string
	DividerID string
}

// ResumeSessionMsg resumes only the explicitly addressed dormant canonical
// session. Empty fields denote a direct slash action; root stamps its origin.
type ResumeSessionMsg struct {
	SessionID       string
	RouteGeneration uint64
}
