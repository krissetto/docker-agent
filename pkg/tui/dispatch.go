package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// The Bubble Tea Update contract returns a tea.Model interface, which forces
// us to type-assert the result back to the concrete sub-model type before
// reassigning it. The helpers below centralise that boilerplate.
//
// Two flavours are provided:
//
//   - updateXCmd: forwards a message to a sub-model and returns the produced
//     tea.Cmd. Use this when several sub-models are updated within the same
//     handler so their commands can be batched.
//
//   - forwardX: same forwarding, but returns (m, cmd) so that handlers that
//     only update a single sub-model can simply `return m.forwardX(msg)`.
//     This avoids the gocritic evalOrder warning that fires on
//     `return m, m.updateX(msg)` because m is mutated by the call.

// updateChatCmd forwards a message to the chat page and returns its cmd.
func (m *appModel) updateChatCmd(msg tea.Msg) tea.Cmd {
	origin := m.paneFocus()
	var generation uint64
	if m.supervisor != nil {
		generation, _ = m.supervisor.RouteGeneration(origin)
	}
	updated, cmd := m.chatPage.Update(msg)
	cmd = stampThinkingCycleCommand(origin, generation, cmd)
	m.chatPage = updated.(chat.Page)
	if id := m.paneFocus(); id != "" {
		m.chatPages[id] = m.chatPage
	}
	if m.panesEnabled() {
		m.syncPaneSidebarSettings()
	}
	if m.panePresentationEnabled() {
		if _, tick := msg.(animation.TickMsg); !tick {
			cmd = tea.Batch(cmd, m.resizePanes())
		}
	}
	return cmd
}

// updateEditorCmd forwards a message to the editor and returns its cmd.
func (m *appModel) updateEditorCmd(msg tea.Msg) tea.Cmd {
	before := m.editor.Value()
	historyNavigation := false
	if key, ok := msg.(tea.KeyPressMsg); ok {
		historyNavigation = (key.String() == "up" || key.String() == "down") && m.editor.HistoryNavigationActive() && !m.editor.IsHistorySearchActive()
	}
	updated, cmd := m.editor.Update(msg)
	cmd = m.stampEditorSend(cmd)
	m.editor = updated.(editor.Editor)
	if before != m.editor.Value() {
		m.editorHistoryValue = ""
		if historyNavigation {
			m.editorHistoryValue = m.editor.Value()
		} else if m.editorHeightMotion.Running() {
			cmd = tea.Batch(cmd, m.resizeAll())
		}
	}
	if before != m.editor.Value() && strings.HasPrefix(m.editor.Value(), "/panes ") && !strings.HasPrefix(before, "/panes ") && m.paneCatalogRequest == nil {
		cmd = tea.Batch(cmd, core.CmdHandler(paneCatalogRefreshMsg{}))
	}
	if id := m.paneFocus(); id != "" {
		m.editors[id] = m.editor
	}
	return cmd
}

// updateDialogCmd forwards a message to the dialog manager and returns its cmd.
func (m *appModel) updateDialogCmd(msg tea.Msg) tea.Cmd {
	if opened, opening := msg.(dialog.OpenDialogMsg); opening {
		for _, page := range m.chatPages {
			if owner, ok := page.(interface{ CancelImagePreviewClick() }); ok {
				owner.CancelImagePreviewClick()
			}
		}
		if hover, ok := m.editor.(editor.BannerHover); ok && hover.CancelBannerHover() {
			m.viewCacheValid = false
		}
		m.cancelInteractionHint()
		if m.panePicker == nil || m.panePicker.dialog != opened.Model {
			m.panePicker = nil
			m.cancelPaneGesture()
		} else {
			m.cancelPaneSource()
		}
		m.isDragging, m.isHoveringHandle = false, false
		if m.tabBar != nil {
			m.tabBar.CancelPointer()
		}
	}
	switch msg.(type) {
	case dialog.CloseDialogMsg, dialog.HideDialogMsg:
		if m.panePicker != nil && m.dialogMgr.TopDialog() == m.panePicker.dialog && !m.panePicker.confirmed {
			m.panePicker = nil
			m.cancelPaneGesture()
		}
	}
	updated, cmd := m.dialogMgr.Update(msg)
	m.dialogMgr = updated.(dialog.Manager)
	if _, opening := msg.(dialog.OpenDialogMsg); opening && m.dialogMgr.Open() && !m.dialogMgr.TopIsBackground() {
		cmds := []tea.Cmd{cmd, chat.ClearSidebarHover(m.chatPage)}
		if m.tabBar != nil {
			cmds = append(cmds, m.tabBar.Update(tea.MouseMotionMsg{X: -1, Y: -1}))
		}
		return tea.Batch(cmds...)
	}
	return cmd
}

// updateCompletionsCmd forwards a message to the completion manager and
// returns its cmd.
func (m *appModel) updateCompletionsCmd(msg tea.Msg) tea.Cmd {
	updated, cmd := m.completions.Update(msg)
	m.completions = updated.(completion.Manager)
	return cmd
}

// forwardChat is a convenience for handlers whose entire response is to
// forward the message to the chat page.
func (m *appModel) forwardChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateChatCmd(msg)
	return m, cmd
}

// forwardEditor is a convenience for handlers whose entire response is to
// forward the message to the editor.
func (m *appModel) forwardEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateEditorCmd(msg)
	return m, cmd
}

// forwardDialog is a convenience for handlers whose entire response is to
// forward the message to the dialog manager.
func (m *appModel) forwardDialog(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateDialogCmd(msg)
	return m, cmd
}

// forwardCompletions is a convenience for handlers whose entire response is
// to forward the message to the completion manager.
func (m *appModel) forwardCompletions(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateCompletionsCmd(msg)
	return m, cmd
}
