package leantui

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/gitbranch"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
)

// viewerHost owns presentation subscriptions, never runtime execution. Hidden
// viewers keep receiving their own events so returning restores a current view.
// Suspended models retain their presentation; fresh viewers are constructed explicitly.
type viewerHost struct {
	themeWatcher        ThemeWatcher
	themeWatcherFactory func(func(string)) ThemeWatcher
	themeRef            string
	themeGeneration     uint64
	closed              bool
	applications        []*app.App
	subscribed          map[*app.App]bool
	store               TabStore
	cold                []tuistate.TabEntry
	restore             func(context.Context, string, string) (*app.App, func(), error)
	ctx                 func() context.Context
	events              chan any
	views               map[*app.App]*model
	back                []*app.App
	cancel              []context.CancelFunc
	cleanup             []func()
}

type viewerEvent struct {
	origin *app.App
	event  any
}

func (h *viewerHost) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.themeWatcher != nil {
		h.themeWatcher.Stop()
		h.themeWatcher = nil
	}
	h.views = nil
	h.back = nil
	for _, cancel := range h.cancel {
		cancel() // detach observers; do not Release or Cancel borrowed handles
	}
	for _, application := range h.applications {
		if application != nil {
			application.Close()
		}
	}
	h.applications = nil
	for _, cleanup := range h.cleanup {
		cleanup()
	}
	h.cleanup = nil
	h.cancel = nil
}

func (m *model) subscribeViewer(ctx context.Context, application *app.App) {
	h := m.viewers
	if h.subscribed == nil {
		h.subscribed = make(map[*app.App]bool)
	}
	if h.subscribed[application] {
		return
	}
	h.subscribed[application] = true
	owned := false
	for _, admitted := range h.applications {
		owned = owned || admitted == application
	}
	if !owned {
		h.applications = append(h.applications, application)
	}
	subctx, cancel := context.WithCancel(ctx)
	h.cancel = append(h.cancel, cancel)
	ready := make(chan struct{})
	go application.Subscribe(subctx, func(event any) {
		select {
		case h.events <- viewerEvent{origin: application, event: event}:
		case <-subctx.Done():
		}
	}, app.SubscribeOptions{PreserveSessionMetadata: true, Ready: ready})
	<-ready
}

func (m *model) routeViewerEvent(ctx context.Context, event any) {
	if result, ok := event.(acquiredSessionView); ok {
		m.finishSessionViewAcquisition(ctx, result)
		return
	}
	if m.viewers != nil && m.viewers.closed {
		return
	}
	if changed, ok := event.(themeFileChanged); ok {
		m.handleThemeFileChanged(changed)
		return
	}
	if routed, ok := event.(viewerEvent); ok {
		if routed.origin == m.app {
			m.handleEvent(ctx, routed.event)
		} else if m.viewers != nil {
			if hidden := m.viewers.views[routed.origin]; hidden != nil {
				hidden.handleEvent(ctx, routed.event)
			}
		}
		return
	}
	m.handleEvent(ctx, event)
}

func (m *model) focusViewer(target *model, remember bool) {
	m.cancelViewAcquisition()
	m.stopSpeech()
	h := m.viewers
	if remember {
		h.back = append(h.back, m.app)
	}
	saved := *m
	h.views[m.app] = &saved
	width, height, renderer, term := m.width, m.height, m.r, m.term
	*m = *target
	m.width, m.height, m.r, m.term, m.viewers = width, height, renderer, term, h
	delete(h.views, m.app)
	m.persistViewerSelection()
	if m.r != nil {
		m.r.Repaint()
	}
}

type subagentViewerLookup interface {
	SubagentAttachInfo(id subagent.NodeID) (runtime.SubagentAttachInfo, bool)
	SubagentNodeForSession(sessionID string) (subagent.NodeID, bool)
}

func (m *model) handleViewerCommand(ctx context.Context, command, id string) {
	if command == "back" {
		if m.viewers == nil || len(m.viewers.back) == 0 {
			m.reportCapability("No previous viewer.", nil)
			return
		}
		h := m.viewers
		previous := h.back[len(h.back)-1]
		h.back = h.back[:len(h.back)-1]
		if target := h.views[previous]; target != nil {
			m.focusViewer(target, false)
		}
		return
	}
	if command == "subagents" || id == "" {
		m.listSubagentViewers()
		return
	}
	lookup, ok := m.app.Runtime().(subagentViewerLookup)
	if !ok {
		m.reportCapability("This runtime does not expose live subagent attachment.", nil)
		return
	}
	// Node IDs are canonical fivehex values. Session IDs are resolved exactly
	// by the runtime; display names and partial UUIDs are never lookup keys.
	nodeID := subagent.NodeID(id)
	if resolved, found := lookup.SubagentNodeForSession(id); found {
		nodeID = resolved
	}
	info, ok := lookup.SubagentAttachInfo(nodeID)
	if !ok || info.Session == nil {
		m.reportCapability("No attachable subagent with that exact node or session ID.", nil)
		return
	}
	if info.Session.ID == m.app.Session().ID {
		m.reportCapability("Already viewing this session.", nil)
		return
	}
	if m.viewers == nil {
		m.reportCapability("Live viewer navigation requires the running lean event loop.", nil)
		return
	}
	for application, target := range m.viewers.views {
		if application.Session().ID == info.Session.ID {
			m.focusViewer(target, true)
			return
		}
	}
	binding := runtime.SessionBinding{AgentName: info.Agent, Model: info.Session.AgentModelOverrides[info.Agent]}
	viewerCtx, cancel := context.WithCancel(m.viewers.ctx())
	application := app.New(viewerCtx, m.app.SessionRuntime(), info.Session, binding,
		app.WithRuntimeServices(m.app.Runtime()), app.WithSubagentAttach(info))
	if application.SessionHandle() == nil {
		cancel()
		m.reportCapability("The runtime could not resolve the subagent session handle.", nil)
		return
	}
	m.viewers.cancel = append(m.viewers.cancel, cancel)
	target := m.newViewer(application, "Message this subagent; /back to return")
	target.loadInitialSessionTranscript()
	m.subscribeViewer(viewerCtx, application)
	application.Start(viewerCtx)
	target.watchViewerBranch(viewerCtx)
	m.focusViewer(target, true)
	m.refreshCommands(ctx)
	m.reportCapability("Live subagent viewer: "+info.Name+" · "+string(info.NodeID)+". Sending targets this session; /back returns without cancelling it.", nil)
}

func (m *model) listSubagentViewers() {
	snapshot := m.subagentSnapshot
	if snapshot == nil {
		snapshot = m.app.Session().GetSubagentTree()
	}
	if snapshot == nil {
		m.reportCapability("No subagents in this session.", nil)
		return
	}
	var choices []ui.Command
	self := subagent.SessionRootID(m.app.Session().ID)
	if attached := m.app.AttachedSubagent(); attached != nil {
		self = attached.NodeID
	}
	var walk func([]subagent.NodeSnapshot, int)
	walk = func(nodes []subagent.NodeSnapshot, depth int) {
		for _, entry := range nodes {
			if entry.Node.ID != self && !strings.HasPrefix(string(entry.Node.ID), "root:") {
				id := string(entry.Node.ID)
				name := entry.Node.DisplayName()
				node, indent := entry.Node, strings.Repeat("  ", depth)
				m.screen.Transcript.AddBlock(func(width int) []string {
					return ui.WrapANSI(indent+styles.AgentIdentityStyle(node.Agent, false).Render(name)+ui.StMuted().Render(fmt.Sprintf(" · %s · %v", id, node.State)), width)
				})
				choices = append(choices, ui.Command{Name: name + " · " + id, Desc: "Live viewer; explicit send targets this session", Value: id})
			}
			walk(entry.Children, depth+1)
		}
	}
	var find func([]subagent.NodeSnapshot) bool
	find = func(nodes []subagent.NodeSnapshot) bool {
		for _, entry := range nodes {
			if entry.Node.ID == self {
				walk(entry.Children, 0)
				return true
			}
			if find(entry.Children) {
				return true
			}
		}
		return false
	}
	find(snapshot.Nodes)
	m.completeArgument("subagent-attach", choices)
}

func (m *model) spawnViewer(ctx context.Context, directory string, fork bool) {
	if m.spawnSession == nil || m.viewers == nil {
		m.reportCapability("Session spawning is not configured by this host.", nil)
		return
	}
	if directory == "" {
		directory = m.app.Session().WorkingDir
	}
	var source *session.Session
	if fork {
		source = m.app.Session()
	}
	application, cleanup, err := m.spawnSession(ctx, directory, source)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	if application == nil || application.Session() == nil || application.SessionHandle() == nil {
		if cleanup != nil {
			cleanup()
		}
		m.reportCapability("Host returned no usable session.", nil)
		return
	}
	m.admitViewer(ctx, application, cleanup)
}

// newViewer borrows only host services and presentation preferences. All session
// state (including interaction drafts, replay cursors and acquisition fences) is
// fresh; no lifecycle state is inherited from the currently focused session.
// The host owns service teardown; the view owns its screen and projection.
func (m *model) newViewer(application *app.App, placeholder string) *model {
	return &model{
		app:              application,
		viewers:          m.viewers,
		sessionViews:     m.sessionViews,
		transcriber:      m.transcriber,
		majorEvents:      m.majorEvents,
		historyStore:     m.historyStore,
		spawnSession:     m.spawnSession,
		runExternal:      m.runExternal,
		plansService:     m.plansService,
		themeResolve:     m.themeResolve,
		themeList:        m.themeList,
		themeLoad:        m.themeLoad,
		themeApply:       m.themeApply,
		settingsSave:     m.settingsSave,
		playSound:        m.playSound,
		soundEnabled:     m.soundEnabled,
		soundThreshold:   m.soundThreshold,
		queueSendMode:    m.queueSendMode,
		interruptMode:    m.interruptMode,
		appName:          m.appName,
		banner:           m.banner,
		disabledCommands: m.disabledCommands,
		renderImages:     m.renderImages,
		hideBanner:       m.hideBanner,
		term:             m.term, r: m.r, width: m.width, height: m.height,
		screen:          ui.NewScreen(application.Session().WorkingDir, "", placeholder, m.historyStore),
		sessionState:    service.NewSessionState(application.Session()),
		usage:           ui.NewUsageTracker(),
		inputReferences: subagentindex.New(),
		status:          ui.StatusModel{WorkingDir: application.Session().WorkingDir, Agent: application.Binding().AgentName},
	}
}

func (m *model) admitViewer(ctx context.Context, application *app.App, cleanup func()) {
	if cleanup != nil {
		m.viewers.cleanup = append(m.viewers.cleanup, cleanup)
	}
	target := m.newViewer(application, "Type a message, / for commands")
	target.loadInitialSessionTranscript()
	m.subscribeViewer(m.viewers.ctx(), application)
	application.Start(m.viewers.ctx())
	target.watchViewerBranch(m.viewers.ctx())
	m.focusViewer(target, true)
	m.refreshCommands(ctx)
	m.reportCapability("Opened session "+application.Session().ID+"; /back returns to the previous viewer.", nil)
}

type viewerBranch string

func (m *model) watchViewerBranch(ctx context.Context) {
	watcher, err := gitbranch.Watch(ctx, m.app.Session().WorkingDir)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	m.status.Branch = watcher.Current()
	origin, host := m.app, m.viewers
	go func() {
		for branch := range watcher.Changes() {
			select {
			case host.events <- viewerEvent{origin: origin, event: viewerBranch(branch)}:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// restoreViewerMetadata hydrates at most the saved active session. Other
// entries remain ordered IDs until explicitly selected, not App/runtime owners.
func (m *model) restoreViewerMetadata(ctx context.Context, cfg Config) {
	if cfg.TabStore == nil {
		return
	}
	if !cfg.RestoreTabs {
		if err := cfg.TabStore.ClearTabs(ctx); err != nil {
			m.reportCapability(nil, err)
		}
	} else {
		entries, active, err := cfg.TabStore.GetTabs(ctx)
		if err != nil {
			m.reportCapability(nil, err)
		} else {
			seen := make(map[string]bool)
			for _, entry := range entries {
				if entry.SessionID == "" || seen[entry.SessionID] {
					continue
				}
				seen[entry.SessionID] = true
				m.restoredEntries = append(m.restoredEntries, entry)
				if entry.SessionID != active || active == m.app.Session().ID || cfg.RestoreSession == nil {
					continue
				}
				application, cleanup, err := cfg.RestoreSession(ctx, entry.SessionID, entry.WorkingDir)
				if err != nil {
					m.reportCapability(nil, err)
					continue
				}
				if application == nil || application.Session() == nil || application.Session().ID != entry.SessionID || application.SessionHandle() == nil {
					if cleanup != nil {
						cleanup()
					}
					m.reportCapability("Could not restore the saved active session; using the initial session.", nil)
					continue
				}
				if m.viewers != nil {
					owned := false
					for _, existing := range m.viewers.applications {
						owned = owned || existing == application
					}
					if !owned {
						m.viewers.applications = append(m.viewers.applications, application)
					}
				}
				m.app = application
				m.status.WorkingDir = application.Session().WorkingDir
				m.sessionState = service.NewSessionState(application.Session())
				if cleanup != nil {
					m.restoredCleanup = append(m.restoredCleanup, cleanup)
				}
			}
		}
	}
	found := false
	for _, entry := range m.restoredEntries {
		found = found || entry.SessionID == m.app.Session().ID
	}
	if !found {
		entry := tuistate.TabEntry{SessionID: m.app.Session().ID, WorkingDir: m.app.Session().WorkingDir}
		m.restoredEntries = append(m.restoredEntries, entry)
		if err := cfg.TabStore.AddTab(ctx, entry.SessionID, entry.WorkingDir); err != nil {
			m.reportCapability(nil, err)
		}
	}
	if err := cfg.TabStore.SetActiveTab(ctx, m.app.Session().ID); err != nil {
		m.reportCapability(nil, err)
	}
}

func (m *model) persistViewerSelection() {
	h := m.viewers
	if h == nil || h.store == nil || m.app.AttachedSubagent() != nil {
		return
	}
	found := false
	for _, entry := range h.cold {
		found = found || entry.SessionID == m.app.Session().ID
	}
	if !found {
		entry := tuistate.TabEntry{SessionID: m.app.Session().ID, WorkingDir: m.app.Session().WorkingDir}
		h.cold = append(h.cold, entry)
		if err := h.store.AddTab(h.ctx(), entry.SessionID, entry.WorkingDir); err != nil {
			m.reportCapability(nil, err)
		}
	}
	if err := h.store.SetActiveTab(h.ctx(), m.app.Session().ID); err != nil {
		m.reportCapability(nil, err)
	}
}

func (m *model) selectRestoredViewer(ctx context.Context, id string) bool {
	h := m.viewers
	if h == nil {
		return false
	}
	if id == m.app.Session().ID {
		return true
	}
	for application, target := range h.views {
		if application.Session().ID == id {
			m.focusViewer(target, true)
			return true
		}
	}
	for _, entry := range h.cold {
		if entry.SessionID != id {
			continue
		}
		if m.sessionViews != nil {
			m.acquireSessionView(ctx, id)
			return true
		}
		if h.restore == nil {
			m.reportCapability("This host cannot restore saved sessions.", nil)
			return true
		}
		application, cleanup, err := h.restore(ctx, id, entry.WorkingDir)
		if err != nil {
			m.reportCapability(nil, err)
			return true
		}
		if application == nil || application.Session() == nil || application.Session().ID != id || application.SessionHandle() == nil {
			if cleanup != nil {
				cleanup()
			}
			m.reportCapability("Host did not restore the requested session.", nil)
			return true
		}
		// Reuse normal admission with the already-resolved host result; it does
		// not call the host a second time or copy a mutable session owner.
		m.admitViewer(ctx, application, cleanup)
		return true
	}
	return false
}

type acquiredSessionView struct {
	origin     *app.App
	identity   app.SessionEventMsg
	generation uint64
	prepared   supervisor.PreparedHostedView
	committed  runtime.CommittedSessionView
	err        error
}

func (m *model) cancelViewAcquisition() {
	m.viewAcquireGeneration++
	if m.viewAcquireCancel != nil {
		m.viewAcquireCancel()
		m.viewAcquireCancel = nil
	}
}

func (m *model) acquireSessionView(ctx context.Context, id string) {
	if m.viewers == nil || m.viewers.closed || m.sessionViews == nil {
		return
	}
	m.cancelViewAcquisition()
	workCtx, cancel := context.WithCancel(ctx)
	m.viewAcquireCancel = cancel
	m.viewers.cancel = append(m.viewers.cancel, cancel)
	origin, identity, generation := m.app, m.app.CurrentSessionEventIdentity(), m.viewAcquireGeneration
	acquirer, host := m.sessionViews, m.viewers
	go func() {
		prepared, err := acquirer.AcquireSessionView(workCtx, id)
		result := acquiredSessionView{origin: origin, identity: identity, generation: generation, prepared: prepared, err: err}
		if err == nil && prepared != nil {
			result.committed, result.err = prepared.Commit(workCtx)
		}
		select {
		case <-workCtx.Done():
			if prepared != nil {
				prepared.Abort()
			}
			return
		default:
		}
		select {
		case host.events <- result:
		case <-workCtx.Done():
			if prepared != nil {
				prepared.Abort()
			}
		}
	}()
}

func (m *model) finishSessionViewAcquisition(ctx context.Context, result acquiredSessionView) {
	if result.prepared != nil {
		defer result.prepared.Abort()
	}
	if m.viewers == nil || m.viewers.closed || m.app != result.origin || m.viewAcquireGeneration != result.generation || !m.app.IsCurrentSessionEvent(result.identity) {
		return
	}
	if result.err != nil {
		m.reportCapability(nil, result.err)
		return
	}
	if result.prepared == nil || result.committed.SessionHandle == nil {
		m.reportCapability("Host returned no canonical session view.", nil)
		return
	}
	application, err := result.prepared.NewApp(m.viewers.ctx(), result.committed)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	if application == nil || application.Session() == nil || application.SessionHandle() != result.committed.SessionHandle {
		if application != nil {
			application.Close()
		}
		m.reportCapability("Host returned an invalid resolved session view.", nil)
		return
	}
	// Runtime resources remain retained by the shared host; this viewer owns
	// only its App presentation and never receives resource cleanup authority.
	m.admitViewer(ctx, application, nil)
}
