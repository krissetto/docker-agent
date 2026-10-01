package messages

import (
	"reflect"
	"slices"
)

// SidebarPosition identifies where the session info sidebar is placed
// relative to the chat area.
type SidebarPosition string

const (
	// SidebarRight is the default vertical sidebar on the right side.
	SidebarRight SidebarPosition = "right"
	// SidebarLeft is a vertical sidebar on the left side.
	SidebarLeft SidebarPosition = "left"
	// SidebarTop is a compact horizontal band above the chat.
	SidebarTop SidebarPosition = "top"
	// SidebarBottom is a compact horizontal band below the chat.
	SidebarBottom SidebarPosition = "bottom"
)

// ParseSidebarPosition normalizes a raw position string, falling back to
// SidebarRight for empty or unknown values so persisted configs can never
// break the layout.
func ParseSidebarPosition(raw string) SidebarPosition {
	switch SidebarPosition(raw) {
	case SidebarLeft, SidebarTop, SidebarBottom:
		return SidebarPosition(raw)
	default:
		return SidebarRight
	}
}

// SectionSpacing identifies how much blank space separates the sidebar
// sections (blocks) in the vertical sidebar.
type SectionSpacing string

const (
	// SpacingNormal is the default spacing between sidebar sections.
	SpacingNormal SectionSpacing = "normal"
	// SpacingCompact tightens the sidebar by using less space between sections.
	SpacingCompact SectionSpacing = "compact"
	// SpacingRelaxed adds extra breathing room between sections.
	SpacingRelaxed SectionSpacing = "relaxed"
)

// ParseSectionSpacing normalizes a raw spacing string, falling back to
// SpacingNormal for empty or unknown values so persisted configs can never
// break the layout.
func ParseSectionSpacing(raw string) SectionSpacing {
	switch SectionSpacing(raw) {
	case SpacingCompact, SpacingRelaxed:
		return SectionSpacing(raw)
	default:
		return SpacingNormal
	}
}

// BlankLines returns the number of blank lines rendered between sidebar
// sections for this spacing.
func (s SectionSpacing) BlankLines() int {
	switch ParseSectionSpacing(string(s)) {
	case SpacingCompact:
		return 1
	case SpacingRelaxed:
		return 3
	default:
		return 2
	}
}

// SidebarInfoMode identifies how the vertical sidebar's Agents section
// renders each agent.
type SidebarInfoMode string

const (
	// InfoModeCompact is the default two-line-per-agent roster.
	InfoModeCompact SidebarInfoMode = "compact"
	// InfoModeDetailed renders each agent as a mini-card with labeled
	// Effort / Context / Cost metrics.
	InfoModeDetailed SidebarInfoMode = "detailed"
)

// ParseSidebarInfoMode normalizes a raw info mode string, falling back to
// InfoModeCompact for empty or unknown values so persisted configs can never
// break the layout.
func ParseSidebarInfoMode(raw string) SidebarInfoMode {
	if SidebarInfoMode(raw) == InfoModeDetailed {
		return InfoModeDetailed
	}
	return InfoModeCompact
}

// LayoutSettings describes the user-customizable TUI layout: where the
// sidebar sits, which of its optional sections are rendered, how much
// space separates them, and how the Agents section renders each agent.
// The zero value is the default layout (sidebar on the right, everything
// visible, normal spacing, compact agent info, full team roster).
type LayoutSettings struct {
	SidebarPosition SidebarPosition
	SectionSpacing  SectionSpacing
	SidebarInfoMode SidebarInfoMode
	// ActiveAgentsOnly filters the sidebar's Agents roster to agents active
	// in the current session. Presentation-only: agent cycling and switching
	// still address the full team.
	ActiveAgentsOnly bool
	HideSessionPath  bool
	HideUsage        bool
	HideAgents       bool
	HideTools        bool
	HideTodos        bool
}

// SendMode identifies what happens to a plain send while the agent is
// working: queued for a later turn or steered into the ongoing stream.
type SendMode string

const (
	// SendModeSteer injects busy sends into the ongoing stream so the agent
	// picks them up mid-turn.
	SendModeSteer SendMode = "steer"
	// SendModeQueue holds busy sends until the current turn ends. This is the default.
	SendModeQueue SendMode = "queue"
)

// ParseSendMode normalizes a raw send mode string, falling back to
// SendModeQueue for empty or unknown values so persisted configs can never
// break message sending.
func ParseSendMode(raw string) SendMode {
	if SendMode(raw) == SendModeSteer {
		return SendModeSteer
	}
	return SendModeQueue
}

// InterruptMode identifies how Esc interrupts a running stream.
type InterruptMode string

const (
	// InterruptModeAlways shows a confirmation dialog on Esc.
	InterruptModeAlways InterruptMode = "always"
	// InterruptModeDoubleTap requires pressing Esc twice within 1 second.
	InterruptModeDoubleTap InterruptMode = "double-tap"
	// InterruptModeNone interrupts immediately on Esc.
	InterruptModeNone InterruptMode = "none"
)

// ParseInterruptMode normalizes a raw interrupt mode string, falling back to
// InterruptModeAlways for empty or unknown values so persisted configs can
// never break the interrupt behavior.
func ParseInterruptMode(raw string) InterruptMode {
	switch InterruptMode(raw) {
	case InterruptModeDoubleTap, InterruptModeNone:
		return InterruptMode(raw)
	default:
		return InterruptModeAlways
	}
}

// PanelElement identifies an independently configurable bottom-panel element.
type PanelElement string

const (
	PanelWorkspace PanelElement = "workspace"
	PanelSubagents PanelElement = "subagents"
	PanelTodos     PanelElement = "todos"
)

// PanelSettings lists enabled elements in display order. A nil slice uses
// defaults; a non-nil empty slice explicitly disables the panel.
type PanelSettings struct {
	Elements []PanelElement
}

// DefaultPanelSettings returns a fresh default configuration.
func DefaultPanelSettings() PanelSettings {
	return PanelSettings{Elements: []PanelElement{PanelWorkspace, PanelSubagents, PanelTodos}}
}

// NormalizePanelSettings clones settings, dropping unknown and duplicate IDs
// without changing order or turning an explicitly empty panel back on.
func NormalizePanelSettings(settings PanelSettings) PanelSettings {
	if settings.Elements == nil {
		return DefaultPanelSettings()
	}
	elements := make([]PanelElement, 0, len(settings.Elements))
	for _, element := range settings.Elements {
		switch element {
		case PanelWorkspace, PanelSubagents, PanelTodos:
			if !slices.Contains(elements, element) {
				elements = append(elements, element)
			}
		}
	}
	return PanelSettings{Elements: elements}
}

// Equal compares normalized enabled elements and their order.
func (p PanelSettings) Equal(other PanelSettings) bool {
	return slices.Equal(NormalizePanelSettings(p).Elements, NormalizePanelSettings(other).Elements)
}

// Equal compares the persistent preferences semantically.
func (p Preferences) Equal(other Preferences) bool {
	if !p.Panel.Equal(other.Panel) {
		return false
	}
	// Keep the scalar preference comparison complete as new fields are added.
	p.Panel, other.Panel = PanelSettings{}, PanelSettings{}
	return reflect.DeepEqual(p, other)
}

// Preferences contains the persistent values managed by the settings dialog.
type Preferences struct {
	Theme                 string
	Layout                LayoutSettings
	Panel                 PanelSettings
	SendMode              SendMode
	SplitDiffView         bool
	ExpandThinking        bool
	HideToolResults       bool
	RenderImages          bool
	ShowBanner            bool
	DimInactivePanes      bool
	TransparentBackground bool
	YOLO                  bool
	RestoreTabs           bool
	Snapshot              bool
	CacheStablePrompts    bool
	WarnOnCacheMiss       bool
	Lean                  bool
	TabTitleMaxLength     int
	Sound                 bool
	SoundThreshold        int
	InterruptConfirmation InterruptMode
}

// Settings dialog messages.
type (
	// OpenSettingsDialogMsg opens the settings dialog (/settings).
	OpenSettingsDialogMsg struct{}

	PreviewSettingsMsg struct {
		TransactionID uint64
		Revision      uint64
		Preferences   Preferences
	}

	CancelSettingsMsg struct {
		TransactionID uint64
		Revision      uint64
	}

	// PreviewLayoutMsg applies layout settings live without persisting them.
	PreviewLayoutMsg struct {
		Layout LayoutSettings
	}

	// PreviewPanelMsg applies panel settings live without persisting them.
	PreviewPanelMsg struct {
		Panel PanelSettings
	}

	// CancelPanelPreviewMsg restores the panel active before a preview.
	CancelPanelPreviewMsg struct {
		Original PanelSettings
	}

	// ApplySettingsMsg applies the settings chosen in the dialog and
	// persists them to the user config.
	ApplySettingsMsg struct {
		TransactionID uint64
		Revision      uint64
		Preferences   Preferences
	}

	// CancelLayoutPreviewMsg restores the layout that was active before a preview.
	CancelLayoutPreviewMsg struct {
		Original LayoutSettings
	}
)
