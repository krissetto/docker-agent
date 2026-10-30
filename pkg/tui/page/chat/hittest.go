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
	TargetSidebarPlan
	TargetSidebarPlanBrowser
	TargetSidebarPlanRefresh
	TargetSidebarUsageContext
	TargetSidebarUsage
	TargetSidebarSubagent
	TargetSidebarBackgroundProcess
	TargetSidebarParent
	TargetSidebarGit
	TargetSidebarContent
	TargetMessages
)

// HitTest determines what UI element is at the given coordinates.
// This centralizes all hit-testing logic in one place, making it easier
// to understand the clickable regions and their priorities.
type HitTest struct {
	page      *chatPage
	AgentName string // populated when At() returns TargetSidebarAgent
	PlanName  string // populated when At() returns TargetSidebarPlan
	// SubagentID is the subagent node id when At() returns
	// TargetSidebarSubagent; ParentSessionID is the parent tab's session id
	// when it returns TargetSidebarParent.
	QueueTurnID          string
	SubagentID           string
	BackgroundToolCallID string
	ParentSessionID      string
	// GitLine is the Git section line when it returns TargetSidebarGit.
	GitLine        string
	OnSubagentName bool
}

// NewHitTest creates a hit tester for the given chat page.
func NewHitTest(page *chatPage) *HitTest {
	return &HitTest{page: page}
}

// At determines what target is at the given coordinates.
// It checks regions in priority order (most specific first).
func (h *HitTest) At(x, y int) MouseTarget {
	p := h.page
	if p.presentationHidden || (p.splitPresentation == nil && y < p.mainContentInset) {
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
		// The shell-owned top inset shifts the vertical edge column as a whole.
		return y == p.mainContentInset && h.isOnSidebarResizeHandle(x, y)
	}

	// Collapsed horizontal: toggle is at right edge of the band's first content line
	if sl.bandContentY(y-p.mainContentInset) != 0 {
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
	handleHeight := max(sl.chatHeight, p.sidebarPresentationExtent)
	if y < p.mainContentInset || y >= p.mainContentInset+handleHeight {
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
	case sidebar.ClickPlan:
		h.PlanName = agentName
		return TargetSidebarPlan
	case sidebar.ClickPlanBrowser:
		return TargetSidebarPlanBrowser
	case sidebar.ClickPlanRefresh:
		return TargetSidebarPlanRefresh
	case sidebar.ClickUsageContext:
		return TargetSidebarUsageContext
	case sidebar.ClickUsage:
		return TargetSidebarUsage
	case sidebar.ClickBackgroundProcess:
		h.BackgroundToolCallID = agentName
		if agentName == "" {
			return TargetSidebarContent
		}
		return TargetSidebarBackgroundProcess
	case sidebar.ClickSubagent:
		h.SubagentID = agentName
		if identities, ok := h.page.sidebar.(interface {
			SubagentIdentityAt(x, y int) (subagent.Node, bool)
		}); ok {
			localX, localY := h.page.sidebarLocalPoint(x, y)
			_, h.OnSubagentName = identities.SubagentIdentityAt(localX, localY)
		}
		return TargetSidebarSubagent
	case sidebar.ClickSubagentParent:
		h.ParentSessionID = agentName
		return TargetSidebarParent
	case sidebar.ClickGit:
		h.GitLine = agentName
		return TargetSidebarGit
	default:
		return TargetSidebarContent
	}
}
