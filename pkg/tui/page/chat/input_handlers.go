package chat

import (
	"errors"
	"log/slog"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// handleKeyPress handles keyboard input events for the chat page.
// Returns the updated model and command. All key presses are handled (forwarded to messages if no match).
func (p *chatPage) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	// When editing title, route keypresses to the sidebar
	if p.sidebar.IsEditingTitle() {
		switch msg.Key().Code {
		case tea.KeyEnter:
			newTitle := p.sidebar.CommitTitleEdit()
			cmd := p.persistSessionTitle(newTitle)
			focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
			return p, tea.Batch(cmd, focusCmd)
		case tea.KeyEscape:
			p.sidebar.CancelTitleEdit()
			return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
		default:
			cmd := p.sidebar.UpdateTitleInput(msg)
			return p, cmd
		}
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("alt+up"))):
		cmd := p.restorePendingMessages()
		return p, cmd

	case key.Matches(msg, p.keyMap.Cancel):
		// If inline editing is active, cancel the edit first
		if p.messages.IsInlineEditing() {
			cmd := p.messages.CancelInlineEdit()
			return p, cmd
		}
		// Otherwise cancel the stream (only if something is running)
		if p.working || p.msgCancel != nil {
			// Response cancellation confirmation belongs to the application shell.
			return p, nil
		}
		// Forward to messages for other uses (e.g., clear selection)
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd

	case key.Matches(msg, p.keyMap.ToggleSplitDiff):
		p.sessionState.ToggleSplitDiffView()
		model, cmd := p.messages.Update(editfile.ToggleDiffViewMsg{})
		p.messages = model.(messages.Model)
		return p, cmd

	case key.Matches(msg, p.keyMap.ToggleSidebar):
		p.sidebar.ToggleCollapsed()
		cmd := p.SetSize(p.width, p.height)
		return p, tea.Batch(cmd, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
	}

	// Route keys to messages (for scrolling, etc.)
	model, cmd := p.messages.Update(msg)
	p.messages = model.(messages.Model)
	return p, cmd
}

// persistSessionTitle saves the new session title to the store
func (p *chatPage) persistSessionTitle(newTitle string) tea.Cmd {
	return func() tea.Msg {
		if err := p.app.UpdateSessionTitle(p.ctx(), newTitle); err != nil {
			// Show warning if title generation is in progress
			if errors.Is(err, app.ErrTitleGenerating) {
				return notification.ShowMsg{Text: "Title is being generated, please wait", Type: notification.TypeWarning}
			}
			slog.Warn("Failed to persist session title", "title", newTitle, "error", err)
			return nil
		}
		return nil
	}
}

// copyWorkingDirToClipboard copies the working directory path to the system clipboard.
func copyWorkingDirToClipboard(wd string) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			_ = clipboard.WriteAll(wd)
			return nil
		},
		tea.SetClipboard(wd),
		notification.SuccessCmd("Working directory copied to clipboard: "+wd),
	)
}

// handleMouseClick handles mouse click events.
func (p *chatPage) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	hit := NewHitTest(p)
	target := hit.At(msg.X, msg.Y)
	sessionID := ""
	if p.app != nil && p.app.Session() != nil {
		sessionID = p.app.Session().ID
	}
	now := time.Now()
	doubleClick := sessionID != "" && msg.Button == tea.MouseLeft && p.lastSidebarClick.sessionID == sessionID && p.lastSidebarClick.target == target && p.lastSidebarClick.turnID == hit.QueueTurnID && now.Sub(p.lastSidebarClick.at) < styles.DoubleClickThreshold
	p.lastSidebarClick = sidebarClick{}
	if msg.Button == tea.MouseLeft && (target == TargetSidebarTitle || target == TargetSidebarQueuedMessage) && !doubleClick {
		p.lastSidebarClick = sidebarClick{sessionID: sessionID, target: target, turnID: hit.QueueTurnID, at: now}
	}

	switch target {
	case TargetSidebarToggle:
		if msg.Button == tea.MouseLeft {
			p.sidebar.ToggleCollapsed()
			cmd := p.SetSize(p.width, p.height)
			return p, tea.Batch(cmd, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}

	case TargetSidebarResizeHandle:
		if msg.Button == tea.MouseLeft {
			p.isDraggingSidebar = true
			p.sidebarDragStartX = msg.X
			p.sidebarDragStartWidth = p.sidebar.GetPreferredWidth()
			p.sidebarDragMoved = false
			return p, nil
		}

	case TargetSidebarStar:
		if msg.Button == tea.MouseLeft {
			sess := p.app.Session()
			if sess != nil {
				return p, core.CmdHandler(msgtypes.ToggleSessionStarMsg{SessionID: sess.ID})
			}
			return p, nil
		}

	case TargetSidebarTitle:
		// Double-click on title to edit
		if msg.Button == tea.MouseLeft {
			if doubleClick {
				p.sidebar.BeginTitleEdit()
				return p, tea.Batch(core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: sessionID}), core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelSidebarTitle}))
			}
			return p, core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: sessionID, Text: "Double-click to rename the session"})
		}

	case TargetSidebarOpenWorkingDir:
		if msg.Button == tea.MouseLeft {
			return p, core.CmdHandler(msgtypes.OpenWorkingDirMsg{Path: p.sidebar.WorkingDirectory()})
		}

	case TargetSidebarWorkingDir:
		if msg.Button == tea.MouseLeft {
			return p, copyWorkingDirToClipboard(p.sidebar.WorkingDirectory())
		}

	case TargetSidebarModel:
		if msg.Button == tea.MouseLeft {
			return p, core.CmdHandler(msgtypes.OpenModelPickerMsg{})
		}

	case TargetSidebarQueuedMessage:
		if msg.Button == tea.MouseLeft {
			if doubleClick {
				for _, queued := range p.messageQueue {
					if queued.turnID == hit.QueueTurnID {
						return p, tea.Batch(core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: sessionID}), core.CmdHandler(msgtypes.OpenPendingEditMsg{SessionID: sessionID, TurnID: queued.turnID, Content: queued.content}))
					}
				}
				return p, nil
			}
			return p, core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: sessionID, Text: "Double-click to edit queued message"})
		}
	case TargetSidebarRemoveQueuedMessage:
		if msg.Button == tea.MouseLeft {
			application, turnID, ctx := p.app, hit.QueueTurnID, p.ctx()
			return p, func() tea.Msg {
				removed, err := application.CancelPendingMessage(ctx, turnID)
				if err != nil {
					return notification.ShowMsg{Text: "Failed to remove queued message: " + err.Error(), Type: notification.TypeError}
				}
				if !removed {
					return notification.ShowMsg{Text: "Queued message is no longer pending", Type: notification.TypeInfo}
				}
				return nil
			}
		}

	case TargetSidebarAgent:
		if cmd := p.agentClickCmd(hit.AgentName, msg.Button, msg.Mod); cmd != nil {
			return p, cmd
		}

	case TargetSidebarUsageContext:
		if msg.Button == tea.MouseLeft {
			return p, core.CmdHandler(msgtypes.ShowContextDialogMsg{})
		}

	case TargetSidebarUsage:
		if msg.Button == tea.MouseLeft {
			return p, core.CmdHandler(msgtypes.ShowCostDialogMsg{})
		}

	case TargetSidebarSubagent:
		if msg.Button == tea.MouseLeft {
			// Navigating to another tab: this sidebar gets no further mouse
			// events, so drop the hover highlight now or the row stays lit
			// until the user returns AND moves the mouse over the sidebar.
			hoverCmd := p.sidebar.ClearSubagentHover()
			return p, tea.Batch(hoverCmd, core.CmdHandler(msgtypes.OpenSubagentMsg{NodeID: hit.SubagentID}))
		}

	case TargetSidebarParent:
		if msg.Button == tea.MouseLeft {
			hoverCmd := p.sidebar.ClearSubagentHover()
			return p, tea.Batch(hoverCmd, core.CmdHandler(msgtypes.SwitchTabMsg{SessionID: hit.ParentSessionID}))
		}

	case TargetMessages:
		if !p.messages.IsMouseOnScrollbar(msg.X, msg.Y) {
			// A click on a subagent tool message ("Spawned x (id)" and
			// friends) opens a tab attached to that subagent's session.
			if msg.Button == tea.MouseLeft {
				if ref, ok := p.messages.InputReferenceAt(msg.X, msg.Y); ok {
					if ref.Kind == lifecycle.InputReferenceParent {
						return p, core.CmdHandler(msgtypes.SwitchTabMsg{SessionID: ref.ID})
					}
					return p, core.CmdHandler(msgtypes.OpenSubagentMsg{NodeID: ref.ID})
				}
			}
			cmd := p.routeMouseEvent(msg, msg.Y)
			focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{
				Target: msgtypes.PanelMessages,
				ClickX: msg.X,
				ClickY: msg.Y,
			})
			return p, tea.Batch(cmd, focusCmd)
		}
	}

	// Default: route to appropriate component
	cmd := p.routeMouseEvent(msg, msg.Y)
	return p, cmd
}

// agentClickCmd resolves a sidebar agent click to its command: a right-click or
// Ctrl+left-click on any agent opens the read-only details dialog; a plain
// left-click switches to it (switching to the already-current agent is a
// harmless no-op). Returns nil when no agent was resolved or the gesture isn't
// one we handle.
func (p *chatPage) agentClickCmd(agentName string, button tea.MouseButton, mod tea.KeyMod) tea.Cmd {
	if agentName == "" {
		return nil
	}
	switch {
	case button == tea.MouseRight, button == tea.MouseLeft && mod == tea.ModCtrl:
		return core.CmdHandler(msgtypes.ShowAgentDetailsMsg{AgentName: agentName})
	case button == tea.MouseLeft:
		return core.CmdHandler(msgtypes.SwitchAgentMsg{AgentName: agentName})
	default:
		return nil
	}
}

// handleMouseMotion handles mouse motion events.
func (p *chatPage) handleMouseMotion(msg tea.MouseMotionMsg) (layout.Model, tea.Cmd) {
	if p.isDraggingSidebar {
		delta := p.sidebarDragStartX - msg.X
		if max(delta, -delta) >= dragThreshold {
			p.sidebarDragMoved = true
		}
		if p.sidebarDragMoved {
			cmd := p.handleSidebarResize(msg.X)
			return p, cmd
		}
		return p, nil
	}

	// During a scrollbar drag, forward motion to both scrollable components
	// so the drag continues even when the cursor drifts outside the component.
	// The scrollbar ignores motion if it isn't the one being dragged.
	if p.isScrollbarDragging() {
		var cmds []tea.Cmd
		messagesModel, messagesCmd := p.messages.Update(msg)
		p.messages = messagesModel.(messages.Model)
		cmds = append(cmds, messagesCmd)

		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)
		cmds = append(cmds, sidebarCmd)
		if p.messages.IsScrollbarDragging() {
			cmds = append(cmds, p.sidebar.ClearSubagentHover())
		}
		return p, tea.Batch(cmds...)
	}

	// During a text-selection drag, keep feeding motion to the messages
	// component even when the cursor drifts over the sidebar, so the
	// selection keeps extending until the button is released.
	if p.messages.IsSelecting() {
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, tea.Batch(cmd, p.sidebar.ClearSubagentHover())
	}

	cmd := p.routeMouseEvent(msg, msg.Y)
	return p, cmd
}

// handleMouseRelease handles mouse release events.
// Release is broadcast to all scrollable components so that a scrollbar drag
// that ends outside the component's bounds still terminates correctly.
func (p *chatPage) handleMouseRelease(msg tea.MouseReleaseMsg) (layout.Model, tea.Cmd) {
	if p.isDraggingSidebar {
		p.isDraggingSidebar = false
		cmd := p.SetSize(p.width, p.height)
		return p, cmd
	}

	var cmds []tea.Cmd

	// Forward release to both messages and sidebar so any active scrollbar
	// drag is properly ended, regardless of where the mouse was released.
	messagesModel, messagesCmd := p.messages.Update(msg)
	p.messages = messagesModel.(messages.Model)
	cmds = append(cmds, messagesCmd)

	sidebarModel, sidebarCmd := p.sidebar.Update(msg)
	p.sidebar = sidebarModel.(sidebar.Model)
	cmds = append(cmds, sidebarCmd)

	return p, tea.Batch(cmds...)
}

// isScrollbarDragging returns true if any scrollable component has an active scrollbar drag.
func (p *chatPage) isScrollbarDragging() bool {
	return p.messages.IsScrollbarDragging() || p.sidebar.IsScrollbarDragging()
}

// handleMouseWheel handles mouse wheel events.
func (p *chatPage) handleWheelCoalesced(msg msgtypes.WheelCoalescedMsg) (layout.Model, tea.Cmd) {
	if msg.Delta == 0 {
		return p, nil
	}
	switch p.wheelTarget(msg.X, msg.Y) {
	case wheelTargetSidebar:
		model, cmd := p.sidebar.Update(msg)
		p.sidebar = model.(sidebar.Model)
		return p, cmd
	default:
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd
	}
}

type wheelTarget int

const (
	wheelTargetMessages wheelTarget = iota
	wheelTargetSidebar
)

func (p *chatPage) wheelTarget(x, _ int) wheelTarget {
	sl := p.computeSidebarLayout()
	if sl.mode == sidebarVertical && !p.sidebar.IsCollapsed() {
		adjustedX := x - styles.AppPadding
		if sl.isInSidebar(adjustedX) {
			return wheelTargetSidebar
		}
	}

	return wheelTargetMessages
}

// handleSidebarResize adjusts sidebar width based on drag position.
func (p *chatPage) handleSidebarResize(x int) tea.Cmd {
	innerWidth := p.width - appPaddingHorizontal
	delta := p.sidebarDragStartX - x
	// A left-side sidebar grows when the handle is dragged right.
	if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
		delta = -delta
	}
	newWidth := p.sidebarDragStartWidth + delta

	// Auto-collapse if dragged below minimum
	if newWidth < sidebar.MinWidth {
		if !p.sidebar.IsCollapsed() {
			// Set preferredWidth to 0 so expanding resets to default
			p.sidebar.SetPreferredWidth(0)
			p.sidebar.SetCollapsed(true)
			return tea.Batch(p.SetSize(p.width, p.height), core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}
		return nil
	}

	// Auto-expand if dragged back above minimum
	var cmds []tea.Cmd
	if p.sidebar.IsCollapsed() {
		p.sidebar.SetCollapsed(false)
		cmds = append(cmds, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
	}

	newWidth = p.sidebar.ClampWidth(newWidth, innerWidth)
	if newWidth != p.sidebar.GetPreferredWidth() {
		p.sidebar.SetPreferredWidth(newWidth)
		cmds = append(cmds, p.SetSize(p.width, p.height))
	}
	return tea.Batch(cmds...)
}
