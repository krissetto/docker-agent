// Package supervisor manages agent sessions.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

// SessionTab represents a running session.
type SessionTab struct {
	ID         string
	App        *app.App
	WorkingDir string
	// title and sessionState mirror the App-owned canonical presentation head.
	title        string
	sessionState runtime.SessionState
	NeedsAttn    bool // True when user attention is needed
	// PendingEvents queues attention events (tool confirmation, max
	// iterations, elicitation) that arrived while this tab was inactive, in
	// arrival order, for replay when the user switches to it. A single slot
	// used to overwrite an earlier event with a later one, silently dropping
	// it (#3584) — e.g. two concurrent background-job elicitations on the
	// same unfocused tab would leave only the second visible.
	PendingEvents   []tea.Msg
	cancel          context.CancelFunc
	lifetimeCtx     context.Context //nolint:containedctx // tab routing follows the managed session lifetime
	routeCancel     context.CancelFunc
	routeDone       <-chan struct{}
	routeGeneration uint64
	projection      *app.PresentationState
	cleanup         func()
}

// RuntimeOwnership declares whether a spawned App borrows the runner's
// existing backend/runtime owner or owns a distinct replacement.
type RuntimeOwnership uint8

const (
	RuntimeBorrowed RuntimeOwnership = iota
	RuntimeOwned
)

// SpawnedSession separates resources from their ownership contract. Cleanup
// is required for RuntimeOwned and ignored for RuntimeBorrowed.
type SpawnedSession struct {
	App       *app.App
	Session   *session.Session
	Ownership RuntimeOwnership
	Cleanup   func()
}

// SessionSpawner creates an App for another session and explicitly declares
// whether it borrows or replaces the current runner's runtime ownership.
type SessionSpawner func(ctx context.Context, workingDir string) (SpawnedSession, error)

// Supervisor manages agent sessions.
type Supervisor struct {
	mu       sync.RWMutex
	runners  map[string]*SessionTab
	order    []string // Maintains tab order
	activeID string
	spawner  SessionSpawner
	program  *tea.Program

	// Tab updates are coalesced behind one serialized sender. Runtime tree
	// snapshots can fan the same invalidation out through several attached Apps;
	// retaining only the newest pending view avoids goroutine growth while the
	// single sender preserves running → terminal delivery order.
	tabSender        func(messages.TabsUpdatedMsg)
	pendingTabUpdate *messages.TabsUpdatedMsg
	tabNotify        chan struct{}
	tabNotifyDone    chan struct{}
	tabNotifyOnce    sync.Once
	tabNotifyStop    sync.Once

	// programReady is closed when SetProgram is called. Subscription goroutines
	// wait on this before consuming events so that startup events (welcome message,
	// agent info, tool info) are not silently dropped.
	programReady     chan struct{}
	programReadyOnce sync.Once
}

// New creates a new supervisor.
func New(spawner SessionSpawner) *Supervisor {
	return &Supervisor{
		runners:       make(map[string]*SessionTab),
		spawner:       spawner,
		programReady:  make(chan struct{}),
		tabNotify:     make(chan struct{}, 1),
		tabNotifyDone: make(chan struct{}),
	}
}

// SetProgram sets the Bubble Tea program for sending messages.
func (s *Supervisor) SetProgram(p *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = p
	s.tabSender = func(msg messages.TabsUpdatedMsg) { p.Send(msg) }
	s.startTabNotifierLocked()
	s.programReadyOnce.Do(func() {
		close(s.programReady)
	})
}

// AddSession adds an existing session to the supervisor.
func (s *Supervisor) AddSession(ctx context.Context, a *app.App, sess *session.Session, workingDir string, cleanup func()) (string, error) {
	s.mu.Lock()
	if _, exists := s.runners[sess.ID]; exists {
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("session %q is already supervised", sess.ID)
	}

	runner := &SessionTab{
		ID:           sess.ID,
		App:          a,
		WorkingDir:   workingDir,
		title:        sess.TitleSnapshot(),
		sessionState: runtime.SessionStateSettled,
		cleanup:      cleanup,
	}

	// Create a cancellable context for this session
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel
	runner.lifetimeCtx = sessionCtx
	if a != nil {
		a.Start(sessionCtx)
	}

	s.runners[sess.ID] = runner
	s.order = append(s.order, sess.ID)

	if s.activeID == "" {
		s.activeID = sess.ID
	}

	// Start the subscription goroutine with routing
	if a != nil {
		runner.routeGeneration++
		generation := runner.routeGeneration
		routeCtx, routeCancel := context.WithCancel(sessionCtx)
		runner.routeCancel = routeCancel
		routeDone := make(chan struct{})
		runner.routeDone = routeDone
		go s.subscribeWithRouting(routeCtx, a, sess.ID, generation, nil, routeDone)
	}
	s.mu.Unlock()

	return sess.ID, nil
}

// SpawnSession creates and adds a new session.
func (s *Supervisor) SpawnSession(ctx context.Context, workingDir string) (string, error) {
	if s.spawner == nil {
		return "", errors.New("session spawning is not available")
	}

	spawned, err := s.spawner(ctx, workingDir)
	if err != nil {
		return "", err
	}
	cleanup := spawned.Cleanup
	if spawned.Ownership == RuntimeBorrowed {
		cleanup = nil
	}

	sessionID, err := s.AddSession(ctx, spawned.App, spawned.Session, workingDir, cleanup)
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

func sessionStateRunning(state runtime.SessionState) bool {
	return state == runtime.SessionStateQueued || state == runtime.SessionStateRunning || state == runtime.SessionStateCancelling
}

func ownSessionActivity(state runtime.SessionState) messages.TabActivity {
	switch state {
	case runtime.SessionStateQueued:
		return messages.TabActivityPending
	case runtime.SessionStateRunning:
		return messages.TabActivityRunning
	default:
		return messages.TabActivityNone
	}
}

// applyPresentation consumes the App's shared head; it never observes or reduces
// the canonical session independently.
func (s *Supervisor) applyPresentation(tabID string, head *app.PresentationState, event runtime.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runner := s.runners[tabID]
	if runner == nil {
		return
	}
	if head == runner.projection {
		switch event.(type) {
		case *app.SessionResetEvent, *app.SessionViewEvent, *runtime.SessionTitleEvent, *runtime.ErrorEvent:
		default:
			return
		}
	}
	if head != nil && head != runner.projection {
		runner.projection = head
		runner.sessionState = head.Status.State
		runner.PendingEvents = slices.DeleteFunc(runner.PendingEvents, func(event tea.Msg) bool {
			key := app.InteractionIdentity(event)
			return key.InteractionID != "" && !head.HasInteraction(key)
		})
		if tabID != s.activeID {
			for _, interaction := range head.Interactions {
				key := app.InteractionIdentity(interaction.Event)
				if interaction.Event != nil && !slices.ContainsFunc(runner.PendingEvents, func(event tea.Msg) bool { return app.InteractionIdentity(event) == key }) {
					runner.PendingEvents = append(runner.PendingEvents, interaction.Event)
				}
			}
		}
		runner.NeedsAttn = len(runner.PendingEvents) > 0
	}
	switch e := event.(type) {
	case *app.SessionResetEvent:
		if e.Snapshot.Session != nil {
			runner.title = e.Snapshot.Session.TitleSnapshot()
		}
	case *app.SessionViewEvent:
		runner.title = e.Session.TitleSnapshot()
	case *runtime.SessionTitleEvent:
		runner.title = e.Title
	case *runtime.ErrorEvent:
		if tabID != s.activeID {
			runner.NeedsAttn = true
		}
	}
	if tabID != s.activeID {
		switch event.(type) {
		case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent, *runtime.ErrorEvent:
			if program := s.program; program != nil {
				go program.Send(messages.BellMsg{})
			}
		}
	}
	if head != nil || event != nil {
		s.notifyTabsUpdated()
	}
}

// subscribeWithRouting subscribes to app events and wraps them with session ID.
// It waits for the program to be set before consuming events so that startup
// events (welcome message, agent/team/tool info) are not dropped.
func (s *Supervisor) subscribeWithRouting(ctx context.Context, a *app.App, sessionID string, generation uint64, ready, done chan<- struct{}) {
	if done != nil {
		defer close(done)
	}
	// Projection delivery must not wait for the Bubble Tea program: tabs can
	// execute and acquire titles before the first window is installed.
	routed := make(chan messages.RoutedMsg, 1024)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		select {
		case <-s.programReady:
		case <-ctx.Done():
			return
		}
		for {
			select {
			case msg := <-routed:
				s.mu.RLock()
				program := s.program
				runner := s.runners[sessionID]
				valid := runner != nil && runner.routeGeneration == generation
				s.mu.RUnlock()
				if program != nil && valid {
					program.Send(msg)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	send := func(msg tea.Msg) {
		s.mu.RLock()
		runner := s.runners[sessionID]
		valid := runner != nil && runner.routeGeneration == generation
		s.mu.RUnlock()
		if !valid || ctx.Err() != nil {
			return
		}
		inner := msg
		if bridged, ok := msg.(app.SessionEventMsg); ok {
			s.applyPresentation(sessionID, bridged.Projection, bridged.Event)
			msg = bridged.Event
			inner = messages.SessionRuntimeEventMsg{Event: bridged.Event, Seed: bridged.Seed, Projection: bridged.Projection}
		}
		s.handleRuntimeEvent(sessionID, msg)
		select {
		case routed <- messages.RoutedMsg{SessionID: sessionID, RouteGeneration: generation, Inner: inner}:
		case <-ctx.Done():
		}
	}

	// Subscriber registration and tab metadata are independent of program readiness.
	subReady := make(chan struct{})
	go func() {
		defer workers.Done()
		a.Subscribe(ctx, func(msg any) { send(msg) }, app.SubscribeOptions{PreserveSessionMetadata: true, Ready: subReady})
	}()
	<-subReady
	if ready != nil {
		close(ready)
	}
	<-ctx.Done()
	workers.Wait()
}

// handleRuntimeEvent updates runner state based on runtime events.
func (s *Supervisor) handleRuntimeEvent(sessionID string, msg tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok {
		return
	}

	if _, ok := msg.(*runtime.SubagentTreeEvent); ok {
		// Topology invalidates descendant presentation only. Session lifecycle is
		// projected exclusively from the canonical session observation below.
		if runner.App != nil && s.runtimeTreeInvalidationOwnerLocked(runner.App.Runtime()) != runner.ID {
			return
		}
		s.notifyTabsUpdated()
	}
}

// notifyTabsUpdated coalesces tab snapshots for one serialized sender (must
// be called with the supervisor lock held). A newer snapshot may replace one
// not yet sent, but can never overtake a snapshot already being sent.
func (s *Supervisor) notifyTabsUpdated() {
	if s.tabSender == nil && s.program != nil {
		p := s.program
		s.tabSender = func(msg messages.TabsUpdatedMsg) { p.Send(msg) }
	}
	if s.tabSender == nil {
		return
	}
	s.startTabNotifierLocked()
	update := messages.TabsUpdatedMsg{
		Tabs:      s.buildTabInfoLocked(),
		ActiveIdx: s.activeIndexLocked(),
	}
	s.pendingTabUpdate = &update
	select {
	case s.tabNotify <- struct{}{}:
	default:
	}
}

func (s *Supervisor) startTabNotifierLocked() {
	if s.tabNotify == nil {
		s.tabNotify = make(chan struct{}, 1)
	}
	if s.tabNotifyDone == nil {
		s.tabNotifyDone = make(chan struct{})
	}
	s.tabNotifyOnce.Do(func() { go s.runTabNotifier() })
}

func (s *Supervisor) runTabNotifier() {
	for {
		select {
		case <-s.tabNotifyDone:
			return
		case <-s.tabNotify:
			for {
				s.mu.Lock()
				pending := s.pendingTabUpdate
				s.pendingTabUpdate = nil
				sender := s.tabSender
				s.mu.Unlock()
				if pending == nil || sender == nil {
					break
				}
				sender(*pending)
			}
		}
	}
}

func (s *Supervisor) runtimeTreeInvalidationOwnerLocked(services app.Services) string {
	fallback := ""
	for _, id := range s.order {
		candidate := s.runners[id]
		if candidate == nil || candidate.App == nil || candidate.App.Runtime() != services {
			continue
		}
		if candidate.App.AttachedSubagent() == nil {
			return id
		}
		if fallback == "" {
			fallback = id
		}
	}
	return fallback
}

// tabActivity derives presentation state from the runtime's current tree.
// The snapshot is synchronous and authoritative, which also seeds tabs opened
// after a tree event and naturally clears state after replacement or teardown.
func tabActivity(runner *SessionTab) (messages.TabActivity, bool) {
	if runner.App == nil {
		return ownSessionActivity(runner.sessionState), false
	}

	attached := runner.App.AttachedSubagent()
	isAttached := attached != nil
	provider, ok := runner.App.Runtime().(interface{ SubagentTree() *subagent.Tree })
	if !ok || provider.SubagentTree() == nil {
		return ownSessionActivity(runner.sessionState), isAttached
	}

	var nodeID subagent.NodeID
	if attached != nil {
		nodeID = attached.NodeID
	} else if sess := runner.App.Session(); sess != nil {
		nodeID = subagent.SessionRootID(sess.ID)
	}
	if nodeID == "" {
		return messages.TabActivityNone, isAttached
	}

	snapshot := provider.SubagentTree().Snapshot()
	return deriveTabActivity(snapshot, nodeID, ownSessionActivity(runner.sessionState)), isAttached
}

func deriveTabActivity(snapshot subagent.Snapshot, nodeID subagent.NodeID, own messages.TabActivity) messages.TabActivity {
	node, found := subagentview.Find(snapshot.Nodes, nodeID)
	if found {
		descendant := strictDescendantState(node)
		if descendant == messages.TabActivityDescendantRunning {
			return descendant
		}
		if own == messages.TabActivityRunning {
			return own
		}
		if descendant == messages.TabActivityPending {
			return descendant
		}
		return own
	}
	return own
}

func strictDescendantState(node subagent.NodeSnapshot) messages.TabActivity {
	pending := false
	var walk func([]subagent.NodeSnapshot) bool
	walk = func(nodes []subagent.NodeSnapshot) bool {
		for _, child := range nodes {
			switch child.Node.State {
			case subagent.NodeRunning:
				return true
			case subagent.NodeStarting:
				pending = true
			}
			if walk(child.Children) {
				return true
			}
		}
		return false
	}
	if walk(node.Children) {
		return messages.TabActivityDescendantRunning
	}
	if pending {
		return messages.TabActivityPending
	}
	return messages.TabActivityNone
}

// buildTabInfoLocked builds tab info (must be called with lock held).
func (s *Supervisor) buildTabInfoLocked() []messages.TabInfo {
	tabs := make([]messages.TabInfo, 0, len(s.order))
	for _, id := range s.order {
		runner := s.runners[id]
		if runner == nil {
			continue
		}

		title := runner.title
		if title == "" {
			title = filepath.Base(runner.WorkingDir)
		}

		activity, attached := tabActivity(runner)
		tabs = append(tabs, messages.TabInfo{
			SessionID:      id,
			Title:          title,
			IsActive:       id == s.activeID,
			IsRunning:      sessionStateRunning(runner.sessionState),
			IsAttached:     attached,
			Activity:       activity,
			NeedsAttention: runner.NeedsAttn,
		})
	}
	return tabs
}

// activeIndexLocked returns the index of the active tab (must be called with lock held).
func (s *Supervisor) activeIndexLocked() int {
	return max(0, slices.Index(s.order, s.activeID))
}

// SwitchTo switches to a different session.
func (s *Supervisor) SwitchTo(sessionID string) *SessionTab {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok {
		return nil
	}

	s.activeID = sessionID
	runner.NeedsAttn = false // Clear attention flag when switching to this tab
	s.notifyTabsUpdated()

	return runner
}

// ConsumePendingEvent pops and returns the oldest pending event for the given
// session (FIFO). Returns nil if none is pending.
func (s *Supervisor) ConsumePendingEvent(sessionID string) tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok || len(runner.PendingEvents) == 0 {
		return nil
	}

	event := runner.PendingEvents[0]
	runner.PendingEvents = runner.PendingEvents[1:]
	return event
}

// SetPendingEvent re-queues an attention event at the FRONT of the given
// session's pending queue, ahead of anything queued behind it, so it can be
// replayed when the user later switches to that tab. Used to re-stash a
// background dialog's originating event when the user navigates away from
// the tab that opened it.
//
// NeedsAttention is intentionally NOT set here: the user is already aware of
// the prompt (they just chose to step away from it) and we don't want to
// flag the tab as if a brand-new event had arrived.
func (s *Supervisor) SetPendingEvent(sessionID string, event tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if runner, ok := s.runners[sessionID]; ok {
		key := app.InteractionIdentity(event)
		if runner.projection != nil && key.InteractionID != "" && !runner.projection.HasInteraction(key) {
			return
		}
		runner.PendingEvents = append([]tea.Msg{event}, runner.PendingEvents...)
	}
}

// ActiveRunner returns the currently active session runner.
func (s *Supervisor) ActiveRunner() *SessionTab {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.runners[s.activeID]
}

// GetRunner returns the runner for the given session ID, or nil if not found.
func (s *Supervisor) GetRunner(sessionID string) *SessionTab {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runners[sessionID]
}

// FindBySession returns the runner whose App currently drives the given
// session, or nil. Unlike GetRunner it matches the App's *current* session:
// a restored tab keeps its original runner key but may drive another session.
func (s *Supervisor) FindBySession(sessionID string) *SessionTab {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, runner := range s.runners {
		if runner.App == nil {
			continue
		}
		if sess := runner.App.Session(); sess != nil && sess.ID == sessionID {
			return runner
		}
	}
	return nil
}

// SeedTitle seeds a lazy tab before its session is attached. Once observation
// starts, canonical snapshot/journal title updates replace this value.
func (s *Supervisor) SeedTitle(sessionID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runner, ok := s.runners[sessionID]; ok {
		runner.title = title
		s.notifyTabsUpdated()
	}
}

// ReplaceRunnerApp replaces the app, working directory, and cleanup function
// for an existing runner. The old app's context is cancelled and its cleanup
// is run asynchronously. A new subscription goroutine is started for the new app.
// This is used when restoring a session whose working directory differs from
// the runner's current one, requiring a fresh runtime.
func (s *Supervisor) ReplaceRunnerApp(ctx context.Context, sessionID string, spawned SpawnedSession, workingDir string) {
	s.mu.Lock()
	runner, ok := s.runners[sessionID]
	if !ok {
		s.mu.Unlock()
		return
	}

	// Cancel old subscription and collect old cleanup.
	if runner.cancel != nil {
		runner.cancel()
	}
	if runner.routeCancel != nil {
		runner.routeCancel()
	}
	oldCleanup := runner.cleanup
	oldApp := runner.App

	// Replace app and working directory. Borrowed replacement explicitly
	// transfers the existing runner ownership; an owned replacement installs
	// its cleanup and retires the prior owner.
	runner.App = spawned.App
	runner.WorkingDir = workingDir
	if spawned.Ownership == RuntimeOwned {
		runner.cleanup = spawned.Cleanup
	}
	runner.sessionState = runtime.SessionStateSettled

	// Create a new cancellable context for the replacement.
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel
	runner.lifetimeCtx = sessionCtx
	if spawned.App != nil {
		spawned.App.Start(sessionCtx)
	}

	s.notifyTabsUpdated()
	s.mu.Unlock()

	if oldApp != nil && oldApp != spawned.App {
		oldApp.Close()
	}
	// Run old cleanup outside the lock only when ownership was replaced.
	if spawned.Ownership == RuntimeOwned && oldCleanup != nil {
		go oldCleanup()
	}

	// Start routing events from the new app.
	if spawned.App != nil {
		s.mu.Lock()
		generation := uint64(0)
		if current := s.runners[sessionID]; current == runner {
			current.routeGeneration++
			generation = current.routeGeneration
		}
		s.mu.Unlock()
		routeCtx, routeCancel := context.WithCancel(sessionCtx)
		s.mu.Lock()
		if current := s.runners[sessionID]; current == runner {
			current.routeCancel = routeCancel
		} else {
			routeCancel()
		}
		s.mu.Unlock()
		go s.subscribeWithRouting(routeCtx, spawned.App, sessionID, generation, nil, nil)
	}
}

// RefreshProjection seeds a retargeted tab from its App's authoritative head.
func (s *Supervisor) RefreshProjection(_ context.Context, sessionID string) {
	s.mu.RLock()
	runner := s.runners[sessionID]
	s.mu.RUnlock()
	if runner != nil && runner.App != nil {
		s.applyPresentation(sessionID, runner.App.Presentation(), nil)
	}
}

func (s *Supervisor) RetargetRunner(ctx context.Context, oldID, newID, workingDir string) bool {
	s.mu.Lock()
	runner := s.runners[oldID]
	if runner == nil || s.runners[newID] != nil {
		s.mu.Unlock()
		return false
	}
	if runner.routeCancel != nil {
		runner.routeCancel()
	}
	runner.routeGeneration++
	generation := runner.routeGeneration
	routeCtx, routeCancel := context.WithCancel(runner.lifetimeCtx)
	runner.routeCancel = routeCancel
	routeDone := make(chan struct{})
	runner.routeDone = routeDone
	delete(s.runners, oldID)
	runner.ID = newID
	runner.WorkingDir = workingDir
	s.runners[newID] = runner
	for i, id := range s.order {
		if id == oldID {
			s.order[i] = newID
		}
	}
	if s.activeID == oldID {
		s.activeID = newID
	}

	s.notifyTabsUpdated()
	appRef := runner.App
	s.mu.Unlock()
	s.RefreshProjection(routeCtx, newID)
	if appRef != nil {
		ready := make(chan struct{})
		go s.subscribeWithRouting(routeCtx, appRef, newID, generation, ready, routeDone)
		select {
		case <-ready:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (s *Supervisor) RouteGeneration(sessionID string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runner := s.runners[sessionID]
	if runner == nil {
		return 0, false
	}
	return runner.routeGeneration, true
}

// ActiveID returns the ID of the currently active session.
func (s *Supervisor) ActiveID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeID
}

// Spawner returns the session spawner function, or nil if none is configured.
func (s *Supervisor) Spawner() SessionSpawner {
	return s.spawner
}

// CloseSession closes a session and removes it from the supervisor.
func (s *Supervisor) CloseSession(sessionID string) string {
	s.mu.Lock()

	runner, ok := s.runners[sessionID]
	if !ok {
		activeID := s.activeID
		s.mu.Unlock()
		return activeID
	}

	// Cancel the session context
	if runner.cancel != nil {
		runner.cancel()
	}
	if runner.routeCancel != nil {
		runner.routeCancel()
	}
	cleanup := runner.cleanup

	// Remove from maps
	delete(s.runners, sessionID)

	// Remove from order slice, remembering where it was.
	closedIdx := 0
	if i := slices.Index(s.order, sessionID); i >= 0 {
		closedIdx = i
		s.order = slices.Delete(s.order, i, i+1)
	}

	// If this was the active session, switch to the previous tab (or the
	// first one when closing the first tab).
	if s.activeID == sessionID {
		if len(s.order) == 0 {
			s.activeID = ""
		} else {
			s.activeID = s.order[max(closedIdx-1, 0)]
		}
	}

	s.notifyTabsUpdated()
	activeID := s.activeID
	s.mu.Unlock()

	// Detach and run cleanup outside the lock so callbacks cannot deadlock.
	if runner.App != nil {
		runner.App.Close()
	}

	if cleanup != nil {
		go cleanup()
	}

	return activeID
}

// Count returns the number of sessions.
func (s *Supervisor) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runners)
}

// GetTabs returns the current tab info.
func (s *Supervisor) GetTabs() ([]messages.TabInfo, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildTabInfoLocked(), s.activeIndexLocked()
}

// ReorderTab moves the tab at fromIdx to toIdx, shifting others accordingly.
func (s *Supervisor) ReorderTab(fromIdx, toIdx int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fromIdx < 0 || fromIdx >= len(s.order) || toIdx < 0 || toIdx >= len(s.order) || fromIdx == toIdx {
		return
	}

	id := s.order[fromIdx]
	s.order = slices.Delete(s.order, fromIdx, fromIdx+1)
	s.order = slices.Insert(s.order, toIdx, id)
	s.notifyTabsUpdated()
}

// Shutdown closes all sessions.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()

	// Cancel all contexts first, then collect cleanup functions.
	var cleanups []func()
	var apps []*app.App
	for _, runner := range s.runners {
		if runner.App != nil {
			apps = append(apps, runner.App)
		}
		if runner.cancel != nil {
			runner.cancel()
		}
		if runner.routeCancel != nil {
			runner.routeCancel()
		}

		if runner.cleanup != nil {
			cleanups = append(cleanups, runner.cleanup)
		}
	}

	count := len(s.runners)
	s.runners = make(map[string]*SessionTab)
	s.order = nil
	s.activeID = ""
	done := s.tabNotifyDone
	s.mu.Unlock()

	s.tabNotifyStop.Do(func() {
		if done != nil {
			close(done)
		}
	})

	// Fence projections and run cleanups outside the lock.
	for _, a := range apps {
		a.Close()
	}

	for _, cleanup := range cleanups {
		cleanup()
	}

	slog.Debug("Supervisor shutdown complete", "sessions", count)
}
