package tui

import (
	"context"
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// A source transaction owns preparation only, never the canonical execution
// driver. Every IO phase returns to the event loop for validation before the
// next phase; cancellation after Commit cannot revoke the dormant registry.
type paneSourceTransaction struct {
	context                   func() context.Context
	cancel                    context.CancelFunc
	application               *app.App
	sessionID, target, focus  string
	replaceRoute, destination string
	edge                      splitEdge
	original, next            splitLayout
	order                     []string
	bounds                    splitRect
	generations               map[string]uint64
	picker                    dialog.Dialog
	prepared                  supervisor.PreparedHostedView
	committed                 *runtime.CommittedSessionView
}

type paneSourcePreparedMsg struct {
	transaction *paneSourceTransaction
	prepared    supervisor.PreparedHostedView
	err         error
}

type paneSourceCommittedMsg struct {
	transaction *paneSourceTransaction
	committed   runtime.CommittedSessionView
	err         error
}

func (m *appModel) cancelPaneSource() {
	if tx := m.paneSource; tx != nil {
		m.paneSource = nil
		tx.cancel()
		if tx.prepared != nil {
			tx.prepared.Abort()
		}
		m.viewCacheValid = false
	}
}

func (m *appModel) validPaneSource(tx *paneSourceTransaction) bool {
	if tx == nil || m.paneSource != tx || tx.context().Err() != nil || m.application != tx.application || (tx.replaceRoute != "" && m.pendingRestores[tx.replaceRoute] != tx.sessionID) || m.paneFocus() != tx.focus || m.panes.root != tx.original.root || !slices.Equal(m.paneOrder(), tx.order) {
		return false
	}
	if m.dialogMgr.Open() && (m.dialogMgr.TopDialog() != tx.picker || !m.dialogMgr.Closing()) {
		return false
	}
	_, bounds, ok := m.measurePanes()
	if !ok || bounds != tx.bounds {
		return false
	}
	for id, expected := range tx.generations {
		if generation, exists := m.supervisor.RouteGeneration(id); !exists || generation != expected {
			return false
		}
	}
	return true
}

func (m *appModel) beginPaneSource(sessionID, target string, edge splitEdge) tea.Cmd {
	m.cancelPaneSource()
	if open := m.supervisor.FindBySession(sessionID); open != nil {
		return m.splitPane(open.ID, target, edge)
	}
	original := m.paneLayout()
	next, ok := original.Insert(sessionID, target, edge)
	_, bounds, supported := m.measurePanes()
	if !ok || !supported || next.Compute(bounds, sessionID, paneMinWidth, paneMinHeight).Compact {
		return notification.ErrorCmd("Not enough room: panes require at least 24 columns × 6 rows each")
	}
	m.panes = original
	ctx, cancel := context.WithCancel(m.ctx())
	tx := &paneSourceTransaction{context: func() context.Context { return ctx }, cancel: cancel, application: m.application, sessionID: sessionID, target: target, focus: m.paneFocus(), edge: edge, original: original, next: next, order: m.paneOrder(), bounds: bounds, generations: make(map[string]uint64)}
	if m.dialogMgr.Open() && m.dialogMgr.Closing() {
		tx.picker = m.dialogMgr.TopDialog()
	}
	for _, id := range tx.order {
		tx.generations[id], _ = m.supervisor.RouteGeneration(id)
	}
	m.paneSource = tx
	m.viewCacheValid = false
	owner := m.supervisor
	return func() tea.Msg {
		prepared, err := owner.AcquireSessionView(ctx, sessionID)
		return paneSourcePreparedMsg{transaction: tx, prepared: prepared, err: err}
	}
}

func (m *appModel) beginPendingPaneSource(route, destination, target string, edge splitEdge, next splitLayout, bounds splitRect) tea.Cmd {
	persisted := m.pendingRestores[route]
	cmd := m.beginPaneSource(persisted, target, edge)
	if tx := m.paneSource; tx != nil {
		tx.replaceRoute, tx.destination, tx.next, tx.bounds = route, destination, next, bounds
	}
	return cmd
}

func (m *appModel) finishPaneSourcePrepared(msg paneSourcePreparedMsg) tea.Cmd {
	tx := msg.transaction
	if !m.validPaneSource(tx) {
		if msg.prepared != nil {
			msg.prepared.Abort()
		}
		if m.paneSource == tx {
			m.cancelPaneSource()
		}
		return nil
	}
	if msg.err != nil || msg.prepared == nil {
		m.cancelPaneSource()
		return notification.ErrorCmd(fmt.Sprintf("Cannot prepare pane; layout unchanged: %v", msg.err))
	}
	info := msg.prepared.Info()
	if info.SessionID != tx.sessionID || info.Session == nil || info.Session.ID != tx.sessionID {
		msg.prepared.Abort()
		m.cancelPaneSource()
		return notification.ErrorCmd("Prepared session identity changed; layout unchanged")
	}
	tx.prepared = msg.prepared
	return func() tea.Msg {
		committed, err := msg.prepared.Commit(tx.context())
		return paneSourceCommittedMsg{transaction: tx, committed: committed, err: err}
	}
}

func (m *appModel) finishPaneSourceCommitted(msg paneSourceCommittedMsg) tea.Cmd {
	tx := msg.transaction
	if !m.validPaneSource(tx) {
		if tx != nil && tx.prepared != nil {
			tx.prepared.Abort()
		}
		if m.paneSource == tx {
			m.cancelPaneSource()
		}
		return nil
	}
	info, handle := msg.committed.Info, msg.committed.SessionHandle
	if msg.err != nil || info.SessionID != tx.sessionID || info.Session == nil || info.Session.ID != tx.sessionID || handle == nil || handle.ID() != tx.sessionID || handle.AgentName() != info.Binding.AgentName {
		m.cancelPaneSource()
		return notification.ErrorCmd(fmt.Sprintf("Cannot commit pane; layout unchanged: %v", msg.err))
	}
	tx.committed = &msg.committed
	return m.adoptPaneSource()
}

func (m *appModel) adoptPaneSource() tea.Cmd {
	tx := m.paneSource
	if tx == nil || tx.committed == nil {
		return nil
	}
	if !m.validPaneSource(tx) {
		m.cancelPaneSource()
		return nil
	}
	if m.dialogMgr.Open() {
		return nil // The exact picker close owns a finite shared-clock lease.
	}
	committed := tx.committed
	info := committed.Info
	application, err := tx.prepared.NewApp(m.ctx(), *committed)
	if err != nil {
		m.cancelPaneSource()
		return notification.ErrorCmd("Cannot adopt canonical pane view: " + err.Error())
	}
	id, initCmd, err := m.adoptSessionView(application, info.Session, info.WorkingDir, sessionViewPolicy{replaceRoute: tx.replaceRoute})
	if err != nil {
		application.Close()
		m.cancelPaneSource()
		return notification.ErrorCmd("Cannot supervise pane view: " + err.Error())
	}
	if tx.replaceRoute != "" {
		delete(m.pendingRestores, id)
	}
	m.capturePaneSidebarSettings()
	m.commitPaneWorkspace(tx.next)
	m.paneSource = nil
	tx.cancel()
	tx.prepared.Abort()
	destination := id
	if tx.destination != "" {
		destination = tx.destination
	}
	_, focus := m.handleSwitchTab(destination)
	return tea.Batch(initCmd, focus)
}
