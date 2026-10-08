package chat

import (
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// MouseTarget represents what the mouse is interacting with.
type MouseTarget int

const (
	TargetNone MouseTarget = iota
	TargetSidebarToggle
	TargetSidebarResizeHandle
	TargetSidebarStar
	TargetSidebarTitle
	TargetSidebarWorkingDir
	TargetSidebarOpenWorkingDir
	TargetSidebarModel
	TargetSidebarThinkingLevel
	TargetSidebarQueuedMessage
	TargetSidebarEditQueuedMessage
	TargetSidebarRemoveQueuedMessage
	TargetSidebarAgent
	TargetSidebarUsageContext
	TargetSidebarUsage
	TargetSidebarSubagent
	TargetSidebarParent
	TargetSidebarContent
	TargetMessages
)

// HitTest determines what UI element is at the given coordinates.
// This centralizes all hit-testing logic in one place, making it easier
// to understand the clickable regions and their priorities.
type HitTest struct {
	page      *chatPage
	AgentName string // populated when At() returns TargetSidebarAgent
	// SubagentID is the subagent node id when At() returns
	// TargetSidebarSubagent; ParentSessionID is the parent tab's session id
	// when it returns TargetSidebarParent.
	QueueTurnID        string
	SubagentID         string
	ParentSessionID    string
	SubagentNode       subagent.Node
	OnSubagentIdentity bool
}

// NewHitTest creates a hit tester for the given chat page.
func NewHitTest(page *chatPage) *HitTest {
	return &HitTest{page: page}
}

// At determines what target is at the given coordinates.
// It checks regions in priority order (most specific first).
func (h *HitTest) At(x, y int) MouseTarget {
	p := h.page
	if p.presentationHidden {
		return TargetNone
	}
	if g := p.splitPresentation; g != nil {
		if p.sidebarInteractive() {
			if g.Shell.SidebarHandle.contains(x, y) {
				if y == g.Shell.SidebarHandle.Y {
					return TargetSidebarToggle
				}
				return TargetSidebarResizeHandle
			}
			if g.Shell.Sidebar.contains(x, y) {
				return h.sidebarClickTarget(x, y)
			}
		}
		if g.Transcript.contains(x, y) {
			return TargetMessages
		}
		return TargetNone
	}

	// Check sidebar toggle glyph
	if h.isOnSidebarToggleGlyph(x, y) {
		return TargetSidebarToggle
	}

	// Check sidebar resize handle
	if h.isOnSidebarResizeHandle(x, y) {
		return TargetSidebarResizeHandle
	}

	// Check sidebar content areas
	sl := p.computeSidebarLayout()
	adjustedX := x - styles.AppPadding

	if sl.mode == sidebarVertical && sl.isInSidebar(adjustedX) {
		return h.sidebarClickTarget(x, y)
	}

	// Check if in collapsed sidebar band (above or below the chat)
	if sl.isInBand(y) {
		return h.sidebarClickTarget(x, y)
	}

	return TargetMessages
}

// isOnSidebarToggleGlyph checks if (x, y) is on the sidebar toggle glyph.
func (h *HitTest) isOnSidebarToggleGlyph(x, y int) bool {
	p := h.page
	sl := p.computeSidebarLayout()

	if !sl.showToggle() {
		return false
	}

	if sl.mode == sidebarVertical {
		// Toggle is at y=0 on the handle column
		return y == 0 && h.isOnSidebarResizeHandle(x, y)
	}

	// Collapsed horizontal: toggle is at right edge of the band's first content line
	if sl.bandContentY(y) != 0 {
		return false
	}
	adjustedX := x - styles.AppPadding
	return adjustedX == sl.innerWidth-toggleColumnWidth
}

// isOnSidebarResizeHandle checks if (x, y) is on the sidebar resize handle column.
func (h *HitTest) isOnSidebarResizeHandle(x, y int) bool {
	p := h.page
	sl := p.computeSidebarLayout()

	if sl.mode != sidebarVertical {
		return false
	}
	if y < 0 || y >= sl.chatHeight {
		return false
	}
	adjustedX := x - styles.AppPadding
	return sl.isOnHandle(adjustedX)
}

// sidebarClickTarget determines the specific target within the sidebar area.
func (h *HitTest) sidebarClickTarget(x, y int) MouseTarget {
	clickResult, agentName := h.page.handleSidebarClickType(x, y)
	switch clickResult {
	case sidebar.ClickStar:
		return TargetSidebarStar
	case sidebar.ClickTitle:
		return TargetSidebarTitle
	case sidebar.ClickOpenWorkingDir:
		return TargetSidebarOpenWorkingDir
	case sidebar.ClickWorkingDir:
		return TargetSidebarWorkingDir
	case sidebar.ClickModel:
		return TargetSidebarModel
	case sidebar.ClickThinkingLevel:
		return TargetSidebarThinkingLevel
	case sidebar.ClickEditQueuedMessage:
		h.QueueTurnID = agentName
		return TargetSidebarEditQueuedMessage
	case sidebar.ClickQueuedMessage:
		h.QueueTurnID = agentName
		return TargetSidebarQueuedMessage
	case sidebar.ClickRemoveQueuedMessage:
		h.QueueTurnID = agentName
		return TargetSidebarRemoveQueuedMessage
	case sidebar.ClickAgent:
		h.AgentName = agentName
		return TargetSidebarAgent
	case sidebar.ClickUsageContext:
		return TargetSidebarUsageContext
	case sidebar.ClickUsage:
		return TargetSidebarUsage
	case sidebar.ClickSubagent:
		h.SubagentID = agentName
		if identities, ok := h.page.sidebar.(interface {
			SubagentIdentityAt(x, y int) (subagent.Node, bool)
		}); ok {
			localX, localY := x-styles.AppPadding-h.page.computeSidebarLayout().sidebarStartX, y
			if g := h.page.splitPresentation; g != nil {
				localX, localY = x-g.Shell.Sidebar.X, y-g.Shell.Sidebar.Y
			}
			h.SubagentNode, h.OnSubagentIdentity = identities.SubagentIdentityAt(localX, localY)
		}
		return TargetSidebarSubagent
	case sidebar.ClickSubagentParent:
		h.ParentSessionID = agentName
		return TargetSidebarParent
	default:
		return TargetSidebarContent
	}
}
