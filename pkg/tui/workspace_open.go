package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type workspaceOpenIntent struct {
	workspaceID             string
	revision                uint64
	target, sessionID, mode string
	origin                  *app.App
	cancel                  context.CancelFunc
}
type workspaceOpenedMsg struct {
	intent *workspaceOpenIntent
	data   *hostedSessionData
	err    error
}

func (m *appModel) cancelWorkspaceOpen() {
	if intent := m.workspaceUI.open; intent != nil {
		intent.cancel()
		m.workspaceUI.open = nil
	}
}
func (m *appModel) openWorkspaceSession(sessionID, mode string) tea.Cmd {
	if sessionID == "" || m.paneWorkspaces.active == nil {
		return nil
	}
	if mode == "other" {
		var items []commands.Item
		for _, workspace := range m.paneWorkspaces.ordered {
			if workspace != m.paneWorkspaces.active {
				destination := workspace.id
				items = append(items, commands.Item{ID: destination, Label: workspace.name, Execute: func(string) tea.Cmd {
					return core.CmdHandler(workspaceOpenDestinationMsg{workspace: destination, sessionID: sessionID})
				}})
			}
		}
		items = append(items, commands.Item{ID: "new", Label: "New workspace", Execute: func(string) tea.Cmd { return core.CmdHandler(workspaceOpenDestinationMsg{sessionID: sessionID}) }})
		return core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPanesDialog("Open in workspace", items)})
	}
	if runner := m.supervisor.FindBySession(sessionID); runner != nil && m.pendingRestores[runner.ID] == "" {
		return m.installWorkspaceRoute(runner.ID, mode)
	}
	m.cancelWorkspaceOpen()
	w := m.paneWorkspaces.active
	ctx, cancel := context.WithCancel(m.ctx())
	intent := &workspaceOpenIntent{workspaceID: w.id, revision: w.revision, target: w.focus, sessionID: sessionID, mode: mode, origin: m.application, cancel: cancel}
	m.workspaceUI.open = intent
	m.workspaceUI.browser.SetLoading(true)
	owner := m.supervisor
	return func() tea.Msg {
		prepared, err := owner.AcquireSessionView(ctx, sessionID)
		if err != nil {
			return workspaceOpenedMsg{intent: intent, err: err}
		}
		committed, err := prepared.Commit(ctx)
		if err != nil {
			prepared.Abort()
			return workspaceOpenedMsg{intent: intent, err: err}
		}
		data := &hostedSessionData{committed: committed, newApp: prepared.NewApp, prepared: prepared}
		return workspaceOpenedMsg{intent: intent, data: data}
	}
}
func (m *appModel) finishWorkspaceOpen(msg workspaceOpenedMsg) tea.Cmd {
	intent := msg.intent
	defer func() {
		if msg.data != nil && msg.data.prepared != nil {
			msg.data.prepared.Abort()
		}
	}()
	w := m.paneWorkspaces.active
	if intent == nil || m.workspaceUI.open != intent || w == nil || w.id != intent.workspaceID || w.revision != intent.revision || w.focus != intent.target || m.application != intent.origin {
		return nil
	}
	m.workspaceUI.open = nil
	defer intent.cancel()
	m.workspaceUI.browser.SetLoading(false)
	if msg.err != nil || msg.data == nil {
		if w.unavailable == nil {
			w.unavailable = make(map[string]string)
		}
		w.unavailable[intent.sessionID] = fmt.Sprint(msg.err)
		return notification.ErrorCmd("Cannot open session: " + fmt.Sprint(msg.err))
	}
	if existing := m.supervisor.FindBySession(intent.sessionID); existing != nil {
		return m.installWorkspaceRoute(existing.ID, intent.mode)
	}
	application, err := msg.data.newApp(m.ctx(), msg.data.committed)
	if err != nil {
		return notification.ErrorCmd("Cannot attach session: " + err.Error())
	}
	sess := application.Session()
	if sess == nil || sess.ID != intent.sessionID {
		application.Close()
		return notification.ErrorCmd("Loaded session identity changed")
	}
	id, err := m.supervisor.AddSession(m.ctx(), application, sess, sess.WorkingDir, nil)
	if err != nil {
		application.Close()
		return notification.ErrorCmd(err.Error())
	}
	m.createSessionComponents(id, application, sess)
	delete(w.unavailable, sess.ID)
	if w.saved != nil {
		w.layout = m.restoreWorkspaceLayout(w.saved, w)
		for _, route := range w.layout.Sessions() {
			m.paneWorkspaces.byRoute[route] = w
		}
		m.panes = w.layout
	}
	cmd := m.installWorkspaceRoute(id, intent.mode)
	return tea.Batch(m.routePaneCmd(id, tea.Batch(m.chatPages[id].Init(), chat.WatchGitBranch(m.chatPages[id]), m.editors[id].Init())), cmd)
}
func (m *appModel) installWorkspaceRoute(id, mode string) tea.Cmd {
	w := m.paneWorkspaces.active
	if w == nil {
		return nil
	}
	m.savePaneWorkspace(m.paneFocus())
	if w.saved != nil {
		delete(w.unavailable, m.persistedSessionID(id))
		w.layout = m.restoreWorkspaceLayout(w.saved, w)
		for _, route := range w.layout.Sessions() {
			if owner := m.paneWorkspaces.byRoute[route]; owner != nil && owner != w {
				m.paneWorkspaces.detach(route)
			}
			m.paneWorkspaces.byRoute[route] = w
		}
	}
	if !w.layout.Contains(id) {
		for _, other := range m.paneWorkspaces.ordered {
			if other != w {
				other.saved = removePersistedLeaf(other.saved, m.persistedSessionID(id))
				delete(other.unavailable, m.persistedSessionID(id))
			}
		}
		m.paneWorkspaces.detach(id)
		if mode == "split" && w.layout.root != nil {
			target := w.focus
			if !w.layout.Contains(target) {
				target = w.layout.Sessions()[0]
			}
			w.layout, _ = w.layout.Insert(id, target, splitRight)
		} else if w.layout.root == nil {
			w.layout = newSplitLayout(id)
		} else {
			target := w.focus
			if !w.layout.Contains(target) {
				target = w.layout.Sessions()[0]
			}
			delete(m.paneWorkspaces.byRoute, target)
			w.layout, _ = w.layout.Replace(target, id)
		}
	}
	w.focus = id
	if mode != "restore" {
		w.desiredFocus = m.persistedSessionID(id)
	}
	w.revision++
	m.paneWorkspaces.byRoute[id] = w
	m.panes = w.layout
	if len(w.unavailable) == 0 {
		w.saved = nil
	}
	m.workspaceUI.browser.Blur()
	m.workspaceUI.visible = !m.workspaceUI.fullscreen
	m.viewCacheValid = false
	_, cmd := m.handleSwitchTab(id)
	return tea.Batch(cmd, m.resizeAll())
}

type workspaceOpenDestinationMsg struct{ workspace, sessionID string }
