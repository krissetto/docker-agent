package tui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/panel"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

type panelSessionData struct {
	epoch                            uint64
	treeDialog                       dialog.Dialog
	todoDialog                       dialog.Dialog
	todoEditor                       dialog.Dialog
	todoRemoval                      dialog.Dialog
	application                      *app.App
	publishedRevision                uint64
	sessionID                        string
	generation, revision             uint64
	workspace                        string
	nodes, treeNodes                 []subagent.NodeSnapshot
	treeLiveObserved                 bool
	todos                            []session.Todo
	todosKnown, requested, attempted bool
}

type panelTodosMsg struct {
	owner, sessionID     string
	generation, revision uint64
	selection            uint64
	todos                []session.Todo
	err                  error
}

func (m *appModel) applyPanelSettings(settings messages.PanelSettings) (tea.Model, tea.Cmd) {
	m.panelSettings = messages.NormalizePanelSettings(settings)
	m.panelFocused = ""
	var focusCmd tea.Cmd
	if m.focusedPanel == PanelStatus {
		m.focusedPanel = PanelEditor
		focusCmd = m.editor.Focus()
	}
	m.viewCacheValid = false
	return m, tea.Batch(focusCmd, m.preparePanel())
}

func (m *appModel) panelOwnerData(owner string) *panelSessionData {
	if m.supervisor == nil {
		return nil
	}
	runner := m.supervisor.GetRunner(owner)
	if runner == nil || runner.App == nil || runner.App.Session() == nil {
		return nil
	}
	sess := runner.App.Session()
	generation, _ := m.supervisor.RouteGeneration(owner)
	if m.panelData == nil {
		m.panelData = make(map[string]*panelSessionData)
	}
	data := m.panelData[owner]
	if data == nil || data.application != runner.App || data.sessionID != sess.ID || data.generation != generation {
		m.closePanelDialogs(data)
		m.panelRevision++
		data = &panelSessionData{epoch: m.panelRevision, application: runner.App, sessionID: sess.ID, generation: generation, revision: m.panelRevision, workspace: sess.WorkingDir}
		if data.workspace == "" {
			data.workspace = m.panelWorkingDir
		}
		if snapshot := sess.GetSubagentTree(); snapshot != nil {
			data.nodes = panelChildren(runner.App, snapshot)
			data.treeNodes = panelTreeNodes(runner.App, snapshot)
		}
		m.panelData[owner] = data
	}
	if sess.WorkingDir != "" {
		data.workspace = sess.WorkingDir
	}
	return data
}

func panelChildren(a *app.App, snapshot *subagent.Snapshot) []subagent.NodeSnapshot {
	if a == nil || a.Session() == nil || snapshot == nil {
		return nil
	}
	var attached subagent.NodeID
	if node := a.AttachedSubagent(); node != nil {
		attached = node.NodeID
	}
	if root, ok := subagentview.Root(*snapshot, a.Session().ID, attached); ok {
		return subagentview.Sorted(root.Children)
	}
	return nil
}

// Keep navigation rooted at the enclosing tree, not the attached viewer's descendants.
func panelTreeNodes(a *app.App, snapshot *subagent.Snapshot) []subagent.NodeSnapshot {
	if a == nil || a.Session() == nil || snapshot == nil {
		return nil
	}
	var attached subagent.NodeID
	if node := a.AttachedSubagent(); node != nil {
		attached = node.NodeID
	}
	focused, ok := subagentview.Root(*snapshot, a.Session().ID, attached)
	if !ok {
		return nil
	}
	for _, root := range snapshot.Nodes {
		if _, found := subagentview.Find([]subagent.NodeSnapshot{root}, focused.Node.ID); found {
			return subagentview.Sorted([]subagent.NodeSnapshot{root})
		}
	}
	return nil
}

func (data *panelSessionData) refreshTree(snapshot *subagent.Snapshot) bool {
	nodes := panelTreeNodes(data.application, snapshot)
	if nodes == nil && len(data.treeNodes) > 0 {
		if root, ok := subagentview.Find(snapshot.Nodes, data.treeNodes[0].Node.ID); ok {
			nodes = subagentview.Sorted([]subagent.NodeSnapshot{root})
		}
	}
	if nodes == nil && !data.treeLiveObserved {
		return false
	}
	if nodes != nil {
		data.treeLiveObserved = true
	}
	data.treeNodes = nodes
	data.nodes = panelChildren(data.application, snapshot)
	return true
}

func (m *appModel) openSubagentsTree() tea.Cmd {
	data := m.panelOwnerData(m.paneFocus())
	if data == nil {
		return nil
	}
	if source, ok := data.application.Runtime().(interface{ SubagentTree() *subagent.Tree }); ok && source.SubagentTree() != nil {
		snapshot := source.SubagentTree().Snapshot()
		if data.treeLiveObserved || panelTreeNodes(data.application, &snapshot) != nil {
			data.refreshTree(&snapshot)
		}
	}
	selected := subagent.NodeID("")
	if node := data.application.AttachedSubagent(); node != nil {
		selected = node.NodeID
	} else if root, ok := subagentview.Root(subagent.Snapshot{Nodes: data.treeNodes}, data.sessionID, ""); ok {
		selected = root.Node.ID
	}
	d := dialog.NewSubagentsDialog(data.treeNodes, m.panelTitles(data.treeNodes), selected)
	d.Update(dialog.SubagentsPolicyMsg{Enabled: subagentsPreference(data.application)})
	data.treeDialog = d
	return tea.Sequence(core.CmdHandler(dialog.OpenDialogMsg{Model: d}), m.loadPanelTitles(d, data))
}

func (m *appModel) ingestPanelEvent(owner string, event runtime.Event) {
	if _, reset := event.(*app.SessionResetEvent); reset {
		m.closePanelDialogs(m.panelData[owner])
		delete(m.panelData, owner)
	}
	data := m.panelOwnerData(owner)
	if data == nil {
		return
	}
	switch event := event.(type) {
	case *app.SessionResetEvent:
		if event.Snapshot.Session != nil {
			data.workspace = event.Snapshot.Session.WorkingDir
			data.nodes = panelChildren(data.application, event.Snapshot.Session.GetSubagentTree())
			data.treeNodes = panelTreeNodes(data.application, event.Snapshot.Session.GetSubagentTree())
		}
	case *app.SessionViewEvent:
		data.workspace = event.Session.WorkingDir
	case *runtime.SubagentTreeEvent:
		if data.refreshTree(&event.Snapshot) && owner == m.paneFocus() {
			m.syncPanelTreeDialog()
		}
	case *runtime.SessionTitleEvent:
		if data.treeDialog != nil && m.dialogMgr.TopDialog() == data.treeDialog {
			m.updateDialogCmd(dialog.SubagentsRefreshMsg{Dialog: data.treeDialog, Titles: map[string]string{event.SessionID: event.Title}})
		}
	case *runtime.TodosChangedEvent:
		m.panelRevision++
		data.revision = m.panelRevision
		data.attempted, data.requested = false, false
	case *runtime.ToolCallResponseEvent:
		if event.ToolDefinition.Category != "todo" || event.Result == nil || event.Result.IsError {
			return
		}
		if handle := data.application.SessionHandle(); handle != nil && handle.Metadata().Capabilities.Todos {
			m.panelRevision++
			data.revision = m.panelRevision
			data.attempted, data.requested = false, false
			return
		}
		if todos, ok := event.Result.Meta.([]session.Todo); ok {
			data.todos = slices.Clone(todos)
			data.todosKnown, data.requested, data.attempted = true, false, true
			m.panelRevision++
			data.revision = m.panelRevision
		}
	}
}

func (m *appModel) preparePanel() tea.Cmd {
	todoCmd := m.prepareTodos()

	if m.focusedPanel == PanelStatus && !m.panelElementVisible(m.panelFocused) {
		m.panelFocused = ""
		m.focusedPanel = PanelEditor
		m.editor.Focus()
		m.viewCacheValid = false
	}
	enabled := len(messages.NormalizePanelSettings(m.panelSettings).Elements) > 0
	if !enabled || m.application == nil || m.messageBar == nil || m.messageBar.Height() == 0 {
		m.panelAnimation.Stop()
		return todoCmd
	}
	owner := m.paneFocus()
	if m.panelSelectionOwner != owner {
		m.panelSelectionOwner = owner
		m.panelSelectionGeneration++
	}
	data := m.panelOwnerData(owner)
	if data == nil {
		m.panelAnimation.Stop()
		return todoCmd
	}
	_, active, _ := panelCounts(data.nodes)
	animate := active > 0 && !m.tickPaused && !m.contextClosed && m.opening == nil && !m.dialogMgr.Open() && m.panelElementVisible(messages.PanelSubagents)
	if animate && !m.panelAnimation.IsActive() {
		m.panelAnimation.SetRuntime(m.ar)
		m.panelAnimation.Start()
	} else if !animate {
		m.panelAnimation.Stop()
	}
	return todoCmd
}

func (m *appModel) acceptPanelTodos(msg panelTodosMsg) tea.Cmd {
	data := m.panelData[msg.owner]
	if data == nil || data.sessionID != msg.sessionID || data.generation != msg.generation || data.revision != msg.revision || !data.requested {
		return nil
	}
	generation, exists := m.supervisor.RouteGeneration(msg.owner)
	if !exists || generation != msg.generation {
		return nil
	}
	data.requested = false
	if msg.selection != 0 && (msg.owner != m.paneFocus() || msg.selection != m.panelSelectionGeneration) {
		data.attempted = false
		return nil
	}
	if msg.err == nil {
		data.todos, data.todosKnown = slices.Clone(msg.todos), true
	}
	m.viewCacheValid = false
	return m.publishTodos(msg.owner, data, msg.err)
}

func panelCounts(nodes []subagent.NodeSnapshot) (total, active, attention int) {
	for _, node := range nodes {
		total++
		if node.Node.State == subagent.NodeRunning && node.Node.WaitingOn == "" {
			active++
		}
		if node.Node.NeedsAttention {
			attention++
		}
		t, a, n := panelCounts(node.Children)
		total += t
		active += a
		attention += n
	}
	return
}

func (m *appModel) panelElements() []panel.Element {
	data := m.panelData[m.paneFocus()]
	if data == nil {
		return nil
	}
	var elements []panel.Element
	for _, id := range messages.NormalizePanelSettings(m.panelSettings).Elements {
		label := ""
		switch id {
		case messages.PanelWorkspace:
			if data.workspace != "" {
				label = filepath.Base(filepath.Clean(data.workspace))
			}
		case messages.PanelSubagents:
			total, active, _ := panelCounts(data.nodes)
			noun := "subagents"
			if total == 1 {
				noun = "subagent"
			}
			label = fmt.Sprintf("%d %s", total, noun)
			if active > 0 {
				label = animation.TabBusy.FrameAt(m.ar.Now()) + " " + fmt.Sprintf("%d/%d %s", active, total, noun)
			}

		case messages.PanelTodos:
			label = "todos unavailable"
			if data.requested {
				label = "todos …"
			}
			if data.todosKnown {
				done := 0
				for _, item := range data.todos {
					if item.Status == "completed" {
						done++
					}
				}
				label = fmt.Sprintf("%d/%d todos", done, len(data.todos))
			}
		}
		elements = append(elements, panel.Element{ID: id, Label: label})
	}
	return elements
}

func (m *appModel) panelFrame() panel.Frame {
	if m.messageBar == nil {
		return panel.Frame{}
	}
	return panel.Render(m.messageBarNotice(), messageBarWidth(m.width), m.panelElements(), m.panelFocused)
}

func (m *appModel) panelElementVisible(id messages.PanelElement) bool {
	for _, zone := range m.panelFrame().Zones {
		if zone.ID == id {
			return true
		}
	}
	return false
}

func (m *appModel) openPanelElement(id messages.PanelElement) tea.Cmd {
	data := m.panelData[m.paneFocus()]
	if data == nil || !m.panelElementVisible(id) {
		return nil
	}
	title := "Workspace"
	lines := []string{data.workspace}
	switch id {
	case messages.PanelSubagents:
		return m.openSubagentsTree()
	case messages.PanelTodos:
		return m.openTodos(messages.OpenTodosMsg{})
	}
	return core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewPanelDetailsDialog(title, lines)})
}

func (m *appModel) focusPanel() bool {
	zones := m.panelFrame().Zones
	if len(zones) == 0 {
		return false
	}
	m.panelFocused = zones[0].ID
	m.focusedPanel = PanelStatus
	m.chatPage.BlurMessages()
	m.editor.Blur()
	return true
}

func (m *appModel) panelKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	if m.focusedPanel != PanelStatus {
		return false, nil
	}
	switch msg.String() {
	case "tab", "esc":
		m.panelFocused = ""
		m.focusedPanel = PanelEditor
		return true, m.editor.Focus()
	case "shift+tab":
		m.panelFocused = ""
		m.focusedPanel = PanelContent
		return true, m.chatPage.FocusMessages()
	case "enter", "space":
		return true, m.openPanelElement(m.panelFocused)
	case "left", "right":
		zones := m.panelFrame().Zones
		if len(zones) == 0 {
			return true, nil
		}
		index := 0
		for i, zone := range zones {
			if zone.ID == m.panelFocused {
				index = i
			}
		}
		if msg.String() == "left" {
			index = (index + len(zones) - 1) % len(zones)
		} else {
			index = (index + 1) % len(zones)
		}
		m.panelFocused = zones[index].ID
		return true, nil
	}
	return false, nil
}

// Keep pause/activity truth ahead of optional panel content.
func (m *appModel) messageBarNotice() string {
	view := m.messageBar.View()
	if m.paneHeaderHeight() == 0 && strings.TrimSpace(ansi.Strip(view)) == "" {
		if m.focusedSessionDormant() || m.sessionState.PauseState() != service.PauseNone {
			view = paneClipped(m.paneActivity(m.paneFocus()), messageBarWidth(m.width), 1)
		} else if m.leanMode && m.chatPage.IsWorking() {
			view = paneClipped(styles.MutedStyle.Render("active"), messageBarWidth(m.width), 1)
		}
	}
	return view
}

func (m *appModel) closePanelDialogs(data *panelSessionData) {
	if data == nil || m.dialogMgr == nil {
		return
	}
	for _, d := range []dialog.Dialog{data.treeDialog, data.todoDialog, data.todoEditor, data.todoRemoval} {
		if d != nil {
			dialog.CleanupDialog(d)
			m.updateDialogCmd(dialog.CloseDialogByModelMsg{Model: d})
		}
	}
}
