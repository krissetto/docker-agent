package tui

import (
	"context"
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// A pending restore is only a presentation transaction. Reading its store does
// not activate a tab, acquire an execution owner, or initialize a page. The
// result is validated on the event loop before any canonical state changes.
type paneHydrationTransaction struct {
	source, destination, target, persisted, focus string
	edge                                          splitEdge
	generation                                    uint64
	destinationGeneration                         uint64
	original, next                                splitLayout
	order                                         []string
	bounds                                        splitRect
	cancel                                        context.CancelFunc
}
type paneHydratedMsg struct {
	transaction *paneHydrationTransaction
	session     *session.Session
	err         error
}

func (m *appModel) supportsPaneCandidate(layout splitLayout, source string) bool {
	if m.leanMode {
		return false
	}
	for _, id := range layout.Sessions() {
		if id == source && m.chatPages[id] == nil {
			runner := m.supervisor.GetRunner(id)
			if runner != nil && runner.App != nil {
				continue
			}
		}
		if _, ok := m.chatPages[id].(chat.SplitPresentation); !ok {
			return false
		}
	}
	return true
}

func (m *appModel) beginPaneHydration(source, destination, target string, edge splitEdge, next splitLayout, bounds splitRect) tea.Cmd {
	runner := m.supervisor.GetRunner(source)
	if m.pendingRestores[source] != "" && (!m.legacyPresentationOnly || runner == nil || runner.App.SessionRuntime() != nil) {
		return m.beginPendingPaneSource(source, destination, target, edge, next, bounds)
	}
	m.cancelPaneGesture()
	runner = m.supervisor.GetRunner(source)
	if runner == nil || runner.App == nil {
		return notification.ErrorCmd("Session unavailable")
	}
	m.panes = m.paneLayout()
	generation, _ := m.supervisor.RouteGeneration(source)
	destinationGeneration, _ := m.supervisor.RouteGeneration(destination)
	ctx, cancel := context.WithCancel(m.ctx())
	tx := &paneHydrationTransaction{source: source, destination: destination, destinationGeneration: destinationGeneration, target: target, persisted: m.pendingRestores[source], focus: m.paneFocus(), edge: edge, generation: generation, original: m.panes, next: next, order: m.paneOrder(), bounds: bounds, cancel: cancel}
	m.paneHydration = tx
	if tx.persisted == "" {
		// A cold but not restored tab already has its canonical session. Defer
		// initialization until the same validated readiness boundary.
		sess := runner.App.Session()
		return func() tea.Msg { return paneHydratedMsg{transaction: tx, session: sess} }
	}
	store := runner.App.SessionStore()
	if store == nil {
		m.cancelPaneGesture()
		return notification.ErrorCmd("Cannot load pane: no session store configured")
	}
	// InMemorySessionStore.GetSession serializes/materializes under its mutex;
	// SQLiteSessionStore reads through concurrency-safe database/sql. Capture
	// store and ID, never read mutable root fields from this command goroutine.
	load := func() tea.Msg {
		sess, err := store.GetSession(ctx, tx.persisted)
		return paneHydratedMsg{transaction: tx, session: sess, err: err}
	}
	m.viewCacheValid = false
	return load
}

func (m *appModel) finishPaneHydration(msg paneHydratedMsg) tea.Cmd {
	tx := msg.transaction
	if tx == nil || m.paneHydration != tx {
		return nil
	}
	generation, exists := m.supervisor.RouteGeneration(tx.source)
	destinationGeneration, destinationExists := m.supervisor.RouteGeneration(tx.destination)
	_, bounds, supported := m.measurePanes()
	if !exists || generation != tx.generation || !destinationExists || destinationGeneration != tx.destinationGeneration || !supported || bounds != tx.bounds ||
		m.paneFocus() != tx.focus || m.panes.root != tx.original.root || !slices.Equal(m.paneOrder(), tx.order) ||
		m.pendingRestores[tx.source] != tx.persisted || m.dialogMgr.Open() ||
		!m.supportsPaneCandidate(tx.next, tx.source) || tx.next.Compute(bounds, tx.destination, paneMinWidth, paneMinHeight).Compact {
		m.cancelPaneGesture()
		return nil
	}
	if msg.err != nil || msg.session == nil {
		m.cancelPaneGesture()
		return notification.ErrorCmd(fmt.Sprintf("Cannot load pane; layout unchanged: %v", msg.err))
	}
	// The read is complete; cancellation now retires only this presentation
	// token. Canonical replacement keeps its normal root context/ownership.
	m.paneHydration = nil
	tx.cancel()
	runner := m.supervisor.GetRunner(tx.source)
	application := runner.App
	if tx.persisted != "" {
		if !m.legacyPresentationOnly || application.SessionRuntime() != nil {
			return notification.ErrorCmd("Persisted pane hydration requires the hosted view path")
		}
		application = app.New(m.ctx(), nil, msg.session, runtime.SessionBinding{AgentName: msg.session.AgentName, Model: msg.session.AgentModelOverrides[msg.session.AgentName]}, app.WithRuntimeServices(application.Runtime()))
		m.supervisor.ReplaceRunnerApp(m.ctx(), tx.source, supervisor.SpawnedSession{App: application, Session: msg.session, Ownership: supervisor.RuntimeBorrowed}, msg.session.WorkingDir)
	}
	if m.chatPages[tx.source] == nil || tx.persisted != "" {
		m.createSessionComponents(tx.source, application, msg.session)
	}
	page, ed := m.chatPages[tx.source], m.editors[tx.source]
	if collapsed, ok := m.pendingSidebarCollapsed[tx.source]; ok {
		page.SetSidebarSettings(chat.SidebarSettings{Collapsed: collapsed})
		delete(m.pendingSidebarCollapsed, tx.source)
	}
	// Exactly one initial hydration; subsequent split/focus operations reuse
	// these canonical objects and never call Init/media discovery again.
	initCmd := tea.Batch(page.Init(), chat.WatchGitBranch(page), ed.Init())
	delete(m.pendingRestores, tx.source)
	m.capturePaneSidebarSettings()
	m.panes = tx.next
	m.viewCacheValid = false
	_, focusCmd := m.handleSwitchTab(tx.destination)
	return tea.Batch(m.routePaneCmd(tx.source, initCmd), focusCmd)
}
