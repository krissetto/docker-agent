package chat

import (
	"errors"
	"log/slog"
	"time"

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
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

// handleKeyPress handles keyboard input events for the chat page.
// Returns the updated model and command. All key presses are handled (forwarded to messages if no match).
func (p *chatPage) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	if p.lastSidebarClick.nodeID != "" {
		p.lastSidebarClick = sidebarClick{}
	}
	p.lastMessageIdentityClick = messageIdentityClick{}
	// When editing title, route keypresses to the sidebar
	if p.sidebarInteractive() && p.sidebar.IsEditingTitle() {
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

	// Inline editing bypasses content shortcuts such as sidebar and pending restore.
	if p.messages.IsInlineEditing() {
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd
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
		p.togglePresentationSidebar()
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
	return core.Sequence(
		func() tea.Msg {
			_ = clipboard.WriteAll(wd)
			return nil
		},
		core.SetClipboard(wd),
		notification.SuccessCmd("Working directory copied to clipboard: "+wd),
	)
}

type messageIdentityClick struct {
	sessionID  string
	ref        lifecycle.InputReference
	occurrence *types.Message
	x, y       int
	at         time.Time
}

// handleMouseClick handles mouse click events.
func (p *chatPage) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	previousMessageClick := p.lastMessageIdentityClick
	p.lastMessageIdentityClick = messageIdentityClick{}
	hit := NewHitTest(p)
	target := hit.At(msg.X, msg.Y)
	if target != TargetSidebarContent || msg.Button != tea.MouseLeft {
		p.sidebar.ResetTodoClick()
	}
	sessionID := ""
	if p.app != nil && p.app.Session() != nil {
		sessionID = p.app.Session().ID
	}
	clickNow := time.Now
	if p.sidebarClickNow != nil {
		clickNow = p.sidebarClickNow
	}
	now := clickNow()
	previousSidebarClick := p.lastSidebarClick
	doubleClick := sessionID != "" && msg.Button == tea.MouseLeft && p.lastSidebarClick.sessionID == sessionID && p.lastSidebarClick.target == target && p.lastSidebarClick.turnID == hit.QueueTurnID && now.Sub(p.lastSidebarClick.at) < styles.DoubleClickThreshold
	p.lastSidebarClick = sidebarClick{}
	if msg.Button == tea.MouseLeft && (target == TargetSidebarTitle || target == TargetSidebarQueuedMessage) && !doubleClick {
		p.lastSidebarClick = sidebarClick{sessionID: sessionID, target: target, turnID: hit.QueueTurnID, at: now}
	}

	switch target {
	case TargetSidebarToggle:
		if msg.Button == tea.MouseLeft {
			p.togglePresentationSidebar()
			cmd := p.SetSize(p.width, p.height)
			return p, tea.Batch(cmd, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}

	case TargetSidebarResizeHandle:
		if msg.Button == tea.MouseLeft {
			p.isDraggingSidebar = true
			p.sidebarDragStartX = msg.X
			p.sidebarDragStartWidth = p.presentationSidebarSettings().PreferredWidth
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

	case TargetSidebarThinkingLevel:
		if msg.Button == tea.MouseLeft {
			cmd := p.cycleThinkingLevelCmd(sessionID)
			return p, cmd
		}

	case TargetSidebarModel:
		if msg.Button == tea.MouseLeft {
			return p, core.CmdHandler(msgtypes.OpenModelPickerMsg{})
		}

	case TargetSidebarQueuedMessage, TargetSidebarEditQueuedMessage:
		if msg.Button == tea.MouseLeft {
			if doubleClick || target == TargetSidebarEditQueuedMessage {
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
			for _, queued := range p.messageQueue {
				if queued.turnID == hit.QueueTurnID {
					return p, core.CmdHandler(msgtypes.OpenPendingRemovalMsg{SessionID: sessionID, TurnID: queued.turnID, Content: queued.content})
				}
			}
			return p, nil
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
			hintSession := p.routingID
			if hintSession == "" {
				hintSession = sessionID
			}
			if node := hit.SubagentNode; node.ID != "" {
				current := sidebarClick{sessionID: sessionID, target: target, nodeID: hit.SubagentID, nodeSessionID: node.SessionID, parentID: string(node.Parent), x: msg.X, y: msg.Y, width: p.width, height: p.height}
				elapsed := now.Sub(previousSidebarClick.at)
				previousSidebarClick.at = time.Time{}
				paired := sessionID != "" && current == previousSidebarClick && elapsed >= 0 && elapsed < styles.DoubleClickThreshold
				if hit.OnSubagentIdentity || paired {
					return p, tea.Batch(p.sidebar.ClearSubagentHover(), core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: hintSession}), core.CmdHandler(msgtypes.OpenSubagentMsg{NodeID: hit.SubagentID}))
				}
				current.at = now
				p.lastSidebarClick = current
				return p, tea.Batch(p.routeMouseEvent(msg, msg.Y), core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: hintSession, Text: "Double-click to attach"}))
			}
			return p, p.routeMouseEvent(msg, msg.Y)
		}

	case TargetSidebarParent:
		if msg.Button == tea.MouseLeft {
			hoverCmd := p.sidebar.ClearSubagentHover()
			return p, tea.Batch(hoverCmd, core.CmdHandler(msgtypes.SwitchTabMsg{SessionID: hit.ParentSessionID}))
		}

	case TargetMessages:
		if !p.messages.IsMouseOnScrollbar(msg.X, msg.Y) {
			// Message cards disclose on the first click; their identity attaches on the second.
			if msg.Button == tea.MouseLeft {
				if ref, ok := p.messages.InputReferenceAt(msg.X, msg.Y); ok {
					var occurrence *types.Message
					if cards, ok := p.messages.(interface {
						AgentMessageIdentityAt(x, y int) (*types.Message, bool)
					}); ok {
						occurrence, _ = cards.AgentMessageIdentityAt(msg.X, msg.Y)
					}
					if occurrence != nil {
						clickNow := time.Now
						if p.messageClickNow != nil {
							clickNow = p.messageClickNow
						}
						at := clickNow()
						elapsed := at.Sub(previousMessageClick.at)
						paired := sessionID != "" && previousMessageClick.occurrence == occurrence && previousMessageClick.sessionID == sessionID && previousMessageClick.ref.Kind == ref.Kind && previousMessageClick.ref.ID == ref.ID && previousMessageClick.x == msg.X && previousMessageClick.y == msg.Y && elapsed >= 0 && elapsed < styles.DoubleClickThreshold
						hintSession := p.routingID
						if hintSession == "" {
							hintSession = sessionID
						}
						if !paired {
							p.lastMessageIdentityClick = messageIdentityClick{sessionID: sessionID, ref: ref, occurrence: occurrence, x: msg.X, y: msg.Y, at: at}
							return p, tea.Batch(p.routeMouseEvent(msg, msg.Y), core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelMessages, ClickX: msg.X, ClickY: msg.Y}), core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: hintSession, Text: "Double-click to attach"}))
						}
						hint := core.CmdHandler(msgtypes.ShowInteractionHintMsg{SessionID: hintSession})
						if ref.Kind == lifecycle.InputReferenceParent {
							return p, tea.Batch(hint, core.CmdHandler(msgtypes.SwitchTabMsg{SessionID: ref.ID}))
						}
						return p, tea.Batch(hint, core.CmdHandler(msgtypes.OpenSubagentMsg{NodeID: ref.ID}))
					}
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
	if p.lastSidebarClick.nodeID != "" && (msg.Button != tea.MouseNone || msg.X != p.lastSidebarClick.x || msg.Y != p.lastSidebarClick.y) {
		p.lastSidebarClick = sidebarClick{}
	}
	if p.isDraggingSidebar {
		p.messages.CancelReferenceHover()
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
		p.messages.CancelReferenceHover()
		var cmds []tea.Cmd
		messagesModel, messagesCmd := p.messages.Update(msg)
		p.messages = messagesModel.(messages.Model)
		cmds = append(cmds, messagesCmd)

		if p.sidebarInteractive() {
			sidebarModel, sidebarCmd := p.sidebar.Update(msg)
			p.sidebar = sidebarModel.(sidebar.Model)
			cmds = append(cmds, sidebarCmd)
		}
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
	if p.lastSidebarClick.nodeID != "" && (msg.X != p.lastSidebarClick.x || msg.Y != p.lastSidebarClick.y) {
		p.lastSidebarClick = sidebarClick{}
	}
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

	if p.sidebarInteractive() {
		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)
		cmds = append(cmds, sidebarCmd)
	}

	return p, tea.Batch(cmds...)
}

// isScrollbarDragging returns true if any scrollable component has an active scrollbar drag.
func (p *chatPage) isScrollbarDragging() bool {
	return p.messages.IsScrollbarDragging() || (p.sidebarInteractive() && p.sidebar.IsScrollbarDragging())
}

// handleMouseWheel handles mouse wheel events.
func (p *chatPage) handleWheelCoalesced(msg msgtypes.WheelCoalescedMsg) (layout.Model, tea.Cmd) {
	if msg.Delta == 0 {
		return p, nil
	}
	switch p.wheelTarget(msg.X, msg.Y) {
	case wheelTargetNone:
		return p, nil
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
	wheelTargetNone
)

func (p *chatPage) wheelTarget(x, y int) wheelTarget {
	if g := p.splitPresentation; g != nil {
		if p.sidebarInteractive() && g.Shell.Sidebar.contains(x, y) {
			return wheelTargetSidebar
		}
		if p.PointerTargetsMessages(x, y) {
			return wheelTargetMessages
		}
		return wheelTargetNone
	}
	sl := p.computeSidebarLayout()
	if sl.mode == sidebarVertical && !p.presentationSidebarSettings().Collapsed {
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

	settings := p.presentationSidebarSettings()
	// Auto-collapse if dragged below minimum, preserving saved preferences
	// when the shell owns a split-layout override.
	if newWidth < sidebar.MinWidth {
		if !settings.Collapsed {
			settings.PreferredWidth = 0
			settings.Collapsed = true
			p.setPresentationSidebarSettings(settings)
			return tea.Batch(p.SetSize(p.width, p.height), core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
		}
		return nil
	}

	var cmds []tea.Cmd
	if settings.Collapsed {
		settings.Collapsed = false
		cmds = append(cmds, core.CmdHandler(msgtypes.ToggleSidebarMsg{}))
	}
	newWidth = p.sidebar.ClampWidth(newWidth, innerWidth)
	changed := settings != p.presentationSidebarSettings() || newWidth != settings.PreferredWidth
	settings.PreferredWidth = newWidth
	if changed {
		p.setPresentationSidebarSettings(settings)
		cmds = append(cmds, p.SetSize(p.width, p.height))
	}
	return tea.Batch(cmds...)
}

func (p *chatPage) cycleThinkingLevelCmd(sessionID string) tea.Cmd {
	owner, ok := p.sidebar.(interface {
		ThinkingTarget() (string, string, bool)
	})
	if !ok || sessionID == "" {
		return nil
	}
	agentName, modelRef, enabled := owner.ThinkingTarget()
	if !enabled || agentName == "" || modelRef == "" {
		return nil
	}
	msg := msgtypes.CycleThinkingLevelMsg{SessionID: sessionID, AgentName: agentName, ModelRef: modelRef}
	if display, ok := p.sidebar.(interface{ ThinkingDisplayReference() string }); ok {
		msg.DisplayedModelRef = display.ThinkingDisplayReference()
	}
	return core.CmdHandler(msg)
}
