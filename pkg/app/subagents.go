package app

import (
	"context"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/subagent"
)

// subagentRuntime retains only the swarm metadata capability that is not
// part of the session API: the live tree the sidebar mirrors.
type subagentRuntime interface {
	SubagentTree() *subagent.Tree
}

// WithSubagentAttach marks the App as a live viewer of an async subagent's
// sub-session. The App mirrors the session's run events onto its bus, and
// hands user input to the runtime for delivery so the subagent processes it
// like any other input.
func WithSubagentAttach(info runtime.SubagentAttachInfo) Opt {
	return func(a *App) {
		a.attachedSubagent = &info
	}
}

// AttachedSubagent returns the attach info when this App is a live viewer of
// an async subagent's sub-session, or nil for a regular App.
func (a *App) AttachedSubagent() *runtime.SubagentAttachInfo {
	return a.attachedSubagent
}

// SessionEventMsg carries bridge-origin metadata for consumers that opt into
// it. Event is intentionally not embedded: ordinary SubscribeWith consumers
// continue receiving the exact runtime.Event values and concrete types they
// received before.
type SessionEventMsg struct {
	Event           runtime.Event
	Seed            bool
	TurnID          string
	OriginSessionID string
	Epoch           uint64
	Sequence        uint64 // per OriginSessionID journal; zero for compatibility/seed events
	Projection      *PresentationState
}

// CurrentSessionEventIdentity captures the attachment identity before starting
// asynchronous work. Carry the token unchanged and check IsCurrentSessionEvent
// when applying its result. Event and non-identity metadata remain zero.
func (a *App) CurrentSessionEventIdentity() SessionEventMsg {
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	identity := SessionEventMsg{Epoch: a.bridgeEpoch.Load()}
	if sess := a.currentState.session; sess != nil {
		identity.OriginSessionID = sess.ID
	}
	return identity
}

// IsCurrentSessionEvent applies the same attachment identity predicate as
// Subscribe at consumption time, including messages buffered by a consumer.
// Epoch zero preserves legacy/synthetic compatibility. Positive epochs must
// match the current bridge and, when a session is installed, its actual ID.
func (a *App) IsCurrentSessionEvent(msg SessionEventMsg) bool {
	if msg.Epoch == 0 {
		return true
	}
	if msg.Epoch != a.bridgeEpoch.Load() {
		return false
	}
	sess := a.Session()
	return sess == nil || msg.OriginSessionID == sess.ID
}

// startSessionEventBridge mirrors the App's session's run events onto the
// App bus — the single event source for local runtimes. Whoever drives a run
// (this App's own turns, the subagent manager for attached children, or the
// runtime's session waking the session with a subagent report), the
// bus carries the same ordered stream. Seed events (the head of an in-flight
// run when subscribing mid-stream) are forwarded first, then the live
// channel. Restartable: a new call replaces the previous session's bridge
// (session switches). Reports whether a bridge is active, so Run knows the
// bus is fed and its own channel is flow-control only.
func (a *App) startSessionEventBridge(ctx context.Context) bool {
	a.initBus(ctx)
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	if a.stopBridge != nil {
		a.stopBridge()
		a.stopBridge = nil
	}
	state := a.state()
	sess, session := state.session, state.handle
	if session == nil || sess == nil {
		return false
	}
	epoch := a.bridgeEpoch.Add(1)
	a.connection.Store(uint32(ConnectionConnecting))
	bridgeCtx, cancel := context.WithCancel(ctx)
	stopBusCancel := context.AfterFunc(a.busLifetime(), cancel)
	attachment, err := runtimeclient.Attach(bridgeCtx, session, &appProjectionSink{app: a, ctx: bridgeCtx, sessionID: sess.ID, epoch: epoch})
	if err != nil {
		stopBusCancel()
		cancel()
		a.sendEvent(ctx, runtime.Error(err.Error()))
		return false
	}
	a.startSubagentTreeWatch(bridgeCtx, session, sess.ID, epoch)
	var stopOnce sync.Once
	a.stopBridge = func() {
		stopOnce.Do(func() {
			stopBusCancel()
			cancel()
			attachment.Detach()
		})
	}
	return true
}

func (a *App) sendBridgedEventFrom(ctx context.Context, requestID string, event runtime.Event, seed bool, originSessionID string, epoch uint64) bool {
	return a.sendSequencedBridgedEventFrom(ctx, requestID, event, seed, originSessionID, epoch, 0)
}

func (a *App) sendSequencedBridgedEventFrom(ctx context.Context, requestID string, event runtime.Event, seed bool, originSessionID string, epoch, sequence uint64) bool {
	if epoch != 0 && epoch != a.bridgeEpoch.Load() {
		return false
	}
	a.projectionMu.Lock()
	if epoch != 0 && epoch != a.bridgeEpoch.Load() {
		a.projectionMu.Unlock()
		return false
	}
	if event = a.filterBridgedEvent(requestID, event); event == nil {
		a.projectionMu.Unlock()
		return true
	}
	projection := a.projectEvent(event)
	if projection != nil && !seed && requestID != "" {
		next := *projection
		switch event.(type) {
		case *runtime.StreamStartedEvent:
			if sequence != 0 && originSessionID == next.Lifecycle.SessionID {
				next.Status.TurnID = requestID
				next.Lifecycle.TurnID = requestID
			}
		case *runtime.StreamStoppedEvent:
			if next.Status.TurnID == requestID && next.Lifecycle.Depth() == 0 && next.Lifecycle.TurnID == "" {
				next.Status.TurnID = ""
			}
		}
		if next.Status.TurnID != projection.Status.TurnID || next.Lifecycle.TurnID != projection.Lifecycle.TurnID {
			projection = &next
			a.presentation.Store(projection)
		}
	}
	a.projectionMu.Unlock()
	originSessionID = strings.TrimSpace(originSessionID)
	select {
	case a.events <- SessionEventMsg{Event: event, Seed: seed, TurnID: requestID, OriginSessionID: originSessionID, Epoch: epoch, Sequence: sequence, Projection: projection}:
		return true
	case <-ctx.Done():
		return false
	}
}

// filterBridgedEvent applies request-correlated App presentation semantics.
// Cancelling one accepted request mutes only that request's tail. Its stop is
// forwarded when no newer request is projected (to clean up the cancelled
// spinner), but a stale stop is suppressed once a newer request's prompt/start
// arrived, so it cannot stop that request's spinner or release its gate.
func (a *App) filterBridgedEvent(requestID string, e runtime.Event) runtime.Event {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	switch e.(type) {
	case *SessionResetEvent, *SessionViewEvent, *ConnectionStateEvent, *runtime.InteractionResolvedEvent, *runtime.TurnSettledEvent, *runtime.SubagentCreatedEvent, *runtime.SubagentTreeEvent, *runtime.DormancyChangedEvent:
		return e
	}
	_, cancelled := a.cancelledRequests[requestID]
	if cancelled {
		if _, stopped := e.(*runtime.StreamStoppedEvent); !stopped {
			return nil
		}
		delete(a.cancelledRequests, requestID)
		if a.projectedRequestID != "" && a.projectedRequestID != requestID {
			return nil
		}
		if a.latestRequestID == requestID {
			a.runCancelled.Store(false)
		}
		a.projectedRequestID = ""
		return e
	}

	switch event := e.(type) {
	case *SessionResetEvent, *runtime.InteractionResolvedEvent, *runtime.PauseChangedEvent, *runtime.PausedEvent, *runtime.SkillOperationEvent:
		// Session state/lifecycle transitions are canonical and must survive
		// request cancellation filtering.
		return e
	case *runtime.SessionTitleEvent:
		if sess := a.Session(); sess == nil || event.SessionID == "" || event.SessionID == sess.ID {
			a.titleGenerating.Store(event.Status == "started")
		}
	case *runtime.StreamStartedEvent:
		a.suppressUserEcho.Store(false)
		if requestID != "" {
			a.projectedRequestID = requestID
		}
	case *runtime.UserMessageEvent:
		if a.suppressUserEcho.Load() {
			return nil
		}
		if requestID != "" {
			a.projectedRequestID = requestID
		}
	case *runtime.StreamStoppedEvent:
		if requestID != "" && a.projectedRequestID != "" && requestID != a.projectedRequestID {
			return nil
		}
		a.projectedRequestID = ""
		if a.latestRequestID == requestID {
			a.runCancelled.Store(false)
		}
		return e
	}
	return e
}

// startSubagentTreeBridge mirrors live swarm snapshots onto the App bus so the
// sidebar can render a running-subagent tree.
func (a *App) startSubagentTreeBridge(ctx context.Context) {
	rt, ok := a.runtime.(subagentRuntime)
	if !ok {
		return
	}
	ch, cancel := rt.SubagentTree().Subscribe(16)
	forwardToBus(ctx, a, nil, ch, cancel, runtime.SubagentTree)
}

// forwardToBus pumps seed then ch onto the App's event bus, wrapping each
// value, until ctx is done or ch closes. A nil wrap result drops the value.
// cancel releases the subscription.
func forwardToBus[T any](ctx context.Context, a *App, seed []T, ch <-chan T, cancel func(), wrap func(T) runtime.Event) {
	go func() {
		defer cancel()
		send := func(v T) bool {
			e := wrap(v)
			if e == nil {
				return true
			}
			select {
			case a.events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, v := range seed {
			if !send(v) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case v, ok := <-ch:
				if !ok || !send(v) {
					return
				}
			}
		}
	}()
}

// emitLiveSubagentTree emits the runtime's current subagent snapshot when it
// covers the active session. An empty or foreign snapshot is not emitted — it
// would wipe a restored view that the live tree knows nothing about.
func (a *App) emitLiveSubagentTree(ctx context.Context) {
	rt, ok := a.runtime.(subagentRuntime)
	sess := a.Session()
	if !ok || sess == nil {
		return
	}
	snap := rt.SubagentTree().Snapshot()
	rootID := subagent.SessionRootID(sess.ID)
	for _, n := range snap.Nodes {
		if n.Node.ID == rootID {
			select {
			case a.events <- runtime.SubagentTree(snap):
			case <-ctx.Done():
			}
			return
		}
	}
}
