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
	owner           *viewOwner
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
	useSubagents         *bool
	mu                   sync.RWMutex
	runners              map[string]*SessionTab
	order                []string // Maintains tab order
	retiredCleanups      []func()
	closed               bool
	viewConfig           *HostViewConfig
	viewContext          func() context.Context
	viewCancel           context.CancelFunc
	viewOwners           map[viewOwnerKey]*viewOwner
	ownerResources       []*viewOwner
	viewLeases           map[*hostedView]struct{}
	ownerOperations      sync.WaitGroup
	presentationClosures sync.WaitGroup
	nextRouteGeneration  uint64 // Unique across tab closure, replacement, and retargeting.
	activeID             string
	spawner              SessionSpawner
	program              *tea.Program

	// Tab updates are coalesced behind one serialized sender. Runtime tree
	// snapshots can fan the same invalidation out through several attached Apps;
	// retaining only the newest pending view avoids goroutine growth while the
	// single sender preserves running → terminal delivery order.
	tabSender     func(messages.TabsUpdatedMsg)
	tabsDirty     bool
	tabNotify     chan struct{}
	tabNotifyDone chan struct{}
	tabNotifyOnce sync.Once
	tabNotifyStop sync.Once

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
	if s.closed || sess == nil {
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return "", runtime.ErrSessionClosed
	}
	if _, exists := s.runners[sess.ID]; exists {
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("session %q is already supervised", sess.ID)
	}

	if a != nil {
		s.applySubagentsPolicyLocked(a.Runtime())
	}
	runner := &SessionTab{
		ID:           sess.ID,
		App:          a,
		WorkingDir:   workingDir,
		title:        sess.TitleSnapshot(),
		sessionState: runtime.SessionStateSettled,
		cleanup:      cleanup,
	}

	if a != nil {
		for _, owner := range s.ownerResources {
			if !owner.removed && sameSessionRuntime(owner.resources.Sessions, a.SessionRuntime()) {
				if owner.resources.Cleanup == nil && cleanup != nil {
					owner.resources.Cleanup = sync.OnceFunc(cleanup)
				}
				runner.owner = owner
				runner.cleanup = nil
				break
			}
		}
	}

	// Create a cancellable context for this session
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel
	runner.lifetimeCtx = sessionCtx
	// Starting an App can enter the runtime; pin the operation and do it only
	// after releasing the lifecycle lock.
	if a != nil {
		s.ownerOperations.Add(1)
	}

	runner.routeGeneration = s.newRouteGenerationLocked()
	s.runners[sess.ID] = runner
	s.order = append(s.order, sess.ID)

	if s.activeID == "" {
		s.activeID = sess.ID
	}

	// Start the subscription goroutine with routing
	if a != nil {
		generation := runner.routeGeneration
		routeCtx, routeCancel := context.WithCancel(sessionCtx)
		runner.routeCancel = routeCancel
		routeDone := make(chan struct{})
		runner.routeDone = routeDone
		go s.subscribeWithRouting(routeCtx, a, sess.ID, generation, nil, routeDone)
	}
	s.mu.Unlock()
	if a != nil {
		a.Start(sessionCtx)
		s.ownerOperations.Done()
	}

	return sess.ID, nil
}

// SpawnSession creates and adds a new session.
func (s *Supervisor) SpawnSession(ctx context.Context, workingDir string) (string, error) {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return "", runtime.ErrSessionClosed
	}
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
func (s *Supervisor) applyPresentation(tabID string, expectedApp *app.App, generation uint64, head *app.PresentationState, event runtime.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runner := s.runners[tabID]; runner != nil && runner.App == expectedApp && runner.routeGeneration == generation {
		s.applyPresentationLocked(runner, head, event)
	}
}

func (s *Supervisor) applyPresentationLocked(runner *SessionTab, head *app.PresentationState, event runtime.Event) {
	tabID := runner.ID
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
		if ctx.Err() != nil {
			return
		}
		inner, valid := s.projectRoutedEvent(sessionID, a, generation, msg)
		if !valid {
			return
		}
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

// projectRoutedEvent fences every supervisor mutation at the same lock boundary
// as route replacement. A subscriber may outlive cancellation, but not its route.
func (s *Supervisor) projectRoutedEvent(sessionID string, expectedApp *app.App, generation uint64, msg tea.Msg) (tea.Msg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runner := s.runners[sessionID]
	if runner == nil || runner.App != expectedApp || runner.routeGeneration != generation {
		return nil, false
	}
	inner := msg
	if bridged, ok := msg.(app.SessionEventMsg); ok {
		s.applyPresentationLocked(runner, bridged.Projection, bridged.Event)
		msg = bridged.Event
		inner = messages.SessionRuntimeEventMsg{Event: bridged.Event, Seed: bridged.Seed, Projection: bridged.Projection, OriginSessionID: bridged.OriginSessionID, TurnID: bridged.TurnID, Epoch: bridged.Epoch, Sequence: bridged.Sequence}
	}
	if _, ok := msg.(*runtime.SubagentTreeEvent); ok {
		// Topology invalidates descendant presentation only, never lifecycle.
		// Observed (remote) trees are per view; a shared live tree notifies once.
		if runner.App == nil || !runner.App.LiveRuntimeTree() || s.runtimeTreeInvalidationOwnerLocked(runner.App.Runtime()) == runner.ID {
			s.notifyTabsUpdated()
		}
	}
	return inner, true
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
	s.tabsDirty = true
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
				var pending *messages.TabsUpdatedMsg
				sender := s.tabSender
				if s.tabsDirty && sender != nil {
					pending = &messages.TabsUpdatedMsg{Tabs: s.buildTabInfoLocked(), ActiveIdx: s.activeIndexLocked()}
				}
				s.tabsDirty = false
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

// tabActivity derives presentation state from the view's current tree.
// The snapshot is synchronous and authoritative, which also seeds tabs opened
// after a tree event and naturally clears state after replacement or teardown.
// attention reports a strict descendant waiting on a human, from the same
// portable tree for in-process and remote owners.
func tabActivity(runner *SessionTab, snapshots map[*subagent.Tree]subagent.Snapshot) (activity messages.TabActivity, attached, attention bool) {
	own := ownSessionActivity(runner.sessionState)
	if runner.App == nil {
		return own, false, false
	}
	attachedNode := runner.App.AttachedSubagent()
	attached = attachedNode != nil
	var nodeID subagent.NodeID
	sess := runner.App.Session()
	if attachedNode != nil {
		nodeID = attachedNode.NodeID
	} else if sess != nil {
		nodeID = subagent.SessionRootID(sess.ID)
	}
	var snapshot subagent.Snapshot
	// One in-process tree is shared by every view of its runtime: read it once per rebuild.
	if provider, ok := runner.App.Runtime().(interface{ SubagentTree() *subagent.Tree }); ok && provider.SubagentTree() != nil {
		tree := provider.SubagentTree()
		cached, found := snapshots[tree]
		if !found {
			cached = tree.Snapshot()
			snapshots[tree] = cached
		}
		snapshot = cached
	} else if view := runner.App.SubagentTreeSnapshot(); view != nil {
		snapshot = *view
	} else {
		return own, attached, false
	}
	if nodeID == "" {
		return messages.TabActivityNone, attached, false
	}
	if sess != nil {
		attention = len(app.DescendantAttention(snapshot, sess.ID)) > 0
	}
	return deriveTabActivity(snapshot, nodeID, own), attached, attention
}

func deriveTabActivity(snapshot subagent.Snapshot, nodeID subagent.NodeID, own messages.TabActivity) messages.TabActivity {
	if own == messages.TabActivityRunning {
		return own
	}
	node, found := subagentview.Find(snapshot.Nodes, nodeID)
	if found {
		descendant := strictDescendantState(node)
		if descendant == messages.TabActivityDescendantRunning {
			return descendant
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
	snapshots := make(map[*subagent.Tree]subagent.Snapshot)
	for _, id := range s.order {
		runner := s.runners[id]
		if runner == nil {
			continue
		}

		title := runner.title
		if title == "" {
			title = filepath.Base(runner.WorkingDir)
		}

		activity, attached, attention := tabActivity(runner, snapshots)
		tabs = append(tabs, messages.TabInfo{
			SessionID:      id,
			Title:          title,
			IsActive:       id == s.activeID,
			IsRunning:      sessionStateRunning(runner.sessionState),
			IsAttached:     attached,
			Activity:       activity,
			NeedsAttention: runner.NeedsAttn || attention || runner.projection.RecoveryUncertain(),
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
	if !ok || s.closed {
		s.mu.Unlock()
		if spawned.Ownership == RuntimeOwned && spawned.Cleanup != nil {
			spawned.Cleanup()
		}
		if spawned.App != nil {
			spawned.App.Close()
		}
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
	oldOwner := runner.owner
	oldApp := runner.App

	// Replace app and working directory. Borrowed replacement explicitly
	// transfers the existing runner ownership; an owned replacement installs
	// its cleanup and retires the prior owner.
	runner.routeGeneration = s.newRouteGenerationLocked()
	generation := runner.routeGeneration
	runner.projection = nil
	runner.PendingEvents = nil
	runner.NeedsAttn = false
	runner.title = ""
	runner.App = spawned.App
	if spawned.App != nil {
		s.applySubagentsPolicyLocked(spawned.App.Runtime())
	}
	runner.WorkingDir = workingDir
	if spawned.Ownership == RuntimeOwned {
		runner.cleanup = spawned.Cleanup
		runner.owner = nil
	}
	if spawned.App != nil {
		for _, owner := range s.ownerResources {
			if !owner.removed && sameSessionRuntime(owner.resources.Sessions, spawned.App.SessionRuntime()) {
				// A compatibility host can register the initial borrowed runtime
				// before a custom spawner transfers its cleanup ownership. Keep
				// that callback in the resource record, not the replaceable view.
				// An existing cleanup remains the sole retirement authority;
				// registered spawners return its normal-close wrapper instead.
				if owner.resources.Cleanup == nil && spawned.Ownership == RuntimeOwned && spawned.Cleanup != nil {
					owner.resources.Cleanup = sync.OnceFunc(spawned.Cleanup)
				}
				runner.owner = owner
				runner.cleanup = nil
				break
			}
		}
	}
	ownerReplaced := oldOwner != runner.owner
	if oldOwner != nil && ownerReplaced {
		oldOwner.closeRequested = true
	}
	if oldOwner != nil && ownerReplaced && !oldOwner.retained && !oldOwner.initial && oldOwner.refs == 0 && !s.ownerHasRunnerLocked(oldOwner) {
		s.removeOwnerLocked(oldOwner)
		oldCleanup = oldOwner.resources.Cleanup
	}
	runner.sessionState = runtime.SessionStateSettled

	// Create a new cancellable context for the replacement.
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel
	runner.lifetimeCtx = sessionCtx
	routeCtx, routeCancel := context.WithCancel(sessionCtx)
	runner.routeCancel = routeCancel
	routeDone := make(chan struct{})
	runner.routeDone = routeDone
	if spawned.App != nil {
		s.ownerOperations.Add(1)
	}

	s.notifyTabsUpdated()
	s.mu.Unlock()
	if spawned.App != nil {
		spawned.App.Start(sessionCtx)
		s.ownerOperations.Done()
	}

	if oldApp != nil && oldApp != spawned.App {
		oldApp.Close()
	}
	// Run old cleanup outside the lock only when ownership was replaced.
	if (spawned.Ownership == RuntimeOwned || ownerReplaced) && oldCleanup != nil {
		go oldCleanup()
	}

	// The identity and cancellation handle were installed with the app above.
	if spawned.App != nil {
		go s.subscribeWithRouting(routeCtx, spawned.App, sessionID, generation, nil, routeDone)
	} else {
		close(routeDone)
	}
}

// RefreshProjection seeds a retargeted tab from its App's authoritative head.
func (s *Supervisor) RefreshProjection(_ context.Context, sessionID string) {
	s.mu.RLock()
	runner := s.runners[sessionID]
	var application *app.App
	var generation uint64
	if runner != nil {
		application, generation = runner.App, runner.routeGeneration
	}
	s.mu.RUnlock()
	if application != nil {
		s.applyPresentation(sessionID, application, generation, application.Presentation(), nil)
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
	runner.routeGeneration = s.newRouteGenerationLocked()
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

func (s *Supervisor) newRouteGenerationLocked() uint64 {
	s.nextRouteGeneration++
	return s.nextRouteGeneration
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

// RetainCleanupUntilShutdown keeps an owned runtime alive after its tab detaches.
func (s *Supervisor) RetainCleanupUntilShutdown(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runner := s.runners[sessionID]; runner != nil {
		if runner.owner != nil {
			// Existing running-child safety ownership is never denied by the
			// optional view cap and never cancelled to make room.
			runner.owner.retained = true
			runner.owner.safety = true
			return
		}
		if runner.cleanup != nil {
			s.retiredCleanups = append(s.retiredCleanups, runner.cleanup)
			runner.cleanup = nil
		}
	}
}

// CloseSession closes a session and removes it from the supervisor.
func (s *Supervisor) CloseSession(sessionID string) string { return s.closeSession(sessionID, false) }

// CloseSessionAsync removes the route immediately; shutdown drains its detached
// presentation before releasing the shared runtime resources.
func (s *Supervisor) CloseSessionAsync(sessionID string) string {
	return s.closeSession(sessionID, true)
}

func (s *Supervisor) closeSession(sessionID string, asynchronous bool) string {
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
	if owner := runner.owner; owner != nil {
		owner.closeRequested = true
		if !owner.retained && !owner.initial && owner.refs == 0 && !s.ownerHasRunnerLocked(owner) {
			s.removeOwnerLocked(owner)
			cleanup = owner.resources.Cleanup
		}
	}

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
	if asynchronous {
		s.presentationClosures.Add(1)
	}
	s.mu.Unlock()

	if asynchronous {
		go func() {
			defer s.presentationClosures.Done()
			if runner.App != nil {
				runner.App.Close()
			}
			if cleanup != nil {
				cleanup()
			}
		}()
		return activeID
	}
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
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancelHost := s.viewCancel
	leases := make([]*hostedView, 0, len(s.viewLeases))
	for lease := range s.viewLeases {
		leases = append(leases, lease)
	}

	// Cancel all contexts first, then collect cleanup functions.
	cleanups := s.retiredCleanups
	s.retiredCleanups = nil
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

	if cancelHost != nil {
		cancelHost()
	}
	// Abort waits for an accepted commit without holding supervisor.mu.
	// Operations that already entered are drained before resource teardown.
	for _, lease := range leases {
		lease.Abort()
	}
	s.ownerOperations.Wait()
	s.presentationClosures.Wait()
	s.mu.Lock()
	for _, owner := range s.ownerResources {
		owner.removed = true
		if owner.resources.Cleanup != nil {
			cleanups = append(cleanups, owner.resources.Cleanup)
		}
	}
	s.ownerResources = nil
	s.viewOwners = nil
	s.mu.Unlock()

	// Fence projections and run cleanups outside the lock.
	for _, a := range apps {
		a.Close()
	}

	for _, cleanup := range cleanups {
		cleanup()
	}

	slog.Debug("Supervisor shutdown complete", "sessions", count)
}
