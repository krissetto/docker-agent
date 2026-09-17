package app

import (
	"context"
	"slices"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type PresentationState = lifecycle.Projection

// SessionResetEvent replaces a consumer's complete projection at an observation barrier.
type SessionResetEvent struct {
	Snapshot runtime.SessionSnapshot
}

func (e *SessionResetEvent) GetAgentName() string { return e.Snapshot.Status.AgentName }
func (e *SessionResetEvent) GetSessionID() string {
	if e.Snapshot.Session != nil {
		return e.Snapshot.Session.ID
	}
	return e.Snapshot.Status.SessionID
}

// SessionViewEvent refreshes owner-edited metadata without replacing live content.
type SessionViewEvent struct{ Session *session.Session }

func (e *SessionViewEvent) GetAgentName() string { return e.Session.AgentName }
func (e *SessionViewEvent) GetSessionID() string { return e.Session.ID }

type InteractionKey = lifecycle.InteractionKey

var InteractionIdentity = lifecycle.InteractionIdentity

var interactionSnapshotKey = lifecycle.InteractionSnapshotKey

// appProjectionSink is the sole presentation reducer for the App and its consumers.
type appProjectionSink struct {
	app       *App
	ctx       context.Context //nolint:containedctx // projection callbacks outlive attachment setup
	sessionID string
	epoch     uint64
	position  int
}

func (s *appProjectionSink) Reset(snapshot runtime.SessionSnapshot) {
	if s.epoch != 0 && s.epoch != s.app.bridgeEpoch.Load() {
		return
	}
	// Empty compatibility observations carry no authoritative state.
	if snapshot.Session == nil && snapshot.Status.SessionID == "" && snapshot.Cursor == 0 && len(snapshot.PendingInputs) == 0 && len(snapshot.Interactions) == 0 {
		return
	}
	if snapshot.Status.SessionID == "" {
		snapshot.Status.SessionID = s.sessionID
	}
	s.position = snapshot.TranscriptPosition
	s.app.sendBridgedEventFrom(s.ctx, "", &SessionResetEvent{Snapshot: snapshot}, true, s.sessionID, s.epoch)
}

func (s *appProjectionSink) Apply(envelope runtime.SessionEvent) {
	if s.epoch != 0 && s.epoch != s.app.bridgeEpoch.Load() {
		return
	}
	withdrawal, canceled := envelope.Event.(*runtime.PendingUserMessageCanceledEvent)
	if canceled && withdrawal.SessionPosition >= 0 && withdrawal.SessionPosition < s.position {
		s.position--
	}
	_, committed := envelope.Event.(*runtime.MessageAddedEvent)
	_, user := envelope.Event.(*runtime.UserMessageEvent)
	positioned := committed || user
	if positioned && envelope.TranscriptPosition >= 0 && envelope.TranscriptPosition < s.position {
		return
	}
	if positioned && envelope.TranscriptPosition >= s.position {
		s.position = envelope.TranscriptPosition + 1
	}
	originSessionID := envelope.SessionID
	if originSessionID == "" {
		originSessionID = s.sessionID // compatibility observations may omit the envelope owner
	}
	s.app.sendSequencedBridgedEventFrom(s.ctx, envelope.TurnID, envelope.Event, false, originSessionID, s.epoch, envelope.Sequence)
}

func (a *App) Presentation() *PresentationState { return a.presentation.Load() }

func (a *App) projectEvent(event runtime.Event) *PresentationState {
	previous := a.presentation.Load()
	if edited, ok := event.(*runtime.PendingUserMessageEditedEvent); ok {
		if sess := a.Session(); sess == nil || edited.SessionID != sess.ID {
			return previous
		}
	}
	if previous != nil {
		a.projectTranscript(event)
	}
	if previous == nil {
		previous = &PresentationState{}
	}
	next := *previous
	switch e := event.(type) {
	case *SessionResetEvent:
		head := &PresentationState{
			Lifecycle:     lifecycle.FromSnapshot(e.Snapshot),
			Status:        e.Snapshot.Status,
			PendingInputs: slices.Clone(e.Snapshot.PendingInputs),
			Interactions:  slices.Clone(e.Snapshot.Interactions),
		}
		if e.Snapshot.Session != nil {
			a.stateMu.Lock()
			view := e.Snapshot.Session.Clone()
			if current := a.currentState.session; current != nil && current.ID == view.ID && view.GetSubagentTree() == nil {
				view.SetSubagentTree(current.GetSubagentTree())
			}
			a.currentState.session = view
			a.stateMu.Unlock()
		}
		a.presentation.Store(head)
		return head
	case *runtime.StreamStartedEvent, *runtime.StreamStoppedEvent, *runtime.PendingUserMessageAcceptedEvent, *runtime.PendingUserMessagePromotedEvent, *runtime.SessionCompactionEvent:
		next.Lifecycle, _ = next.Lifecycle.Apply(event)
		next.Status.State = next.Lifecycle.Status
	case *runtime.PendingUserMessageEditedEvent:
		// Editing changes payload only, not lifecycle or pending membership.
	case *runtime.InteractionResolvedEvent:
		next.Interactions = slices.DeleteFunc(slices.Clone(next.Interactions), func(i runtime.InteractionSnapshot) bool {
			return interactionSnapshotKey(i) == (InteractionKey{SessionID: e.SessionID, InteractionID: e.InteractionID})
		})
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		key := InteractionIdentity(event)
		if next.HasInteraction(key) {
			return previous
		}
		next.Interactions = append(slices.Clone(next.Interactions), runtime.InteractionSnapshot{SessionID: key.SessionID, InteractionID: key.InteractionID, Event: event})
	case *runtime.PendingUserMessageCanceledEvent:
		next.Lifecycle.Pending = slices.DeleteFunc(slices.Clone(next.Lifecycle.Pending), func(id string) bool { return id == e.TurnID })
	case *runtime.DormancyChangedEvent:
		next.Status.Dormant = e.Dormant
	case *runtime.PauseChangedEvent:
		next.Status.PauseArmed = e.Paused
		if !e.Paused {
			next.Status.Paused = false
		}
	case *runtime.PausedEvent:
		next.Status.Paused = true
	default:
		return previous
	}
	switch e := event.(type) {
	case *runtime.PendingUserMessageEditedEvent:
		if index := slices.IndexFunc(next.PendingInputs, func(p runtime.PendingInput) bool { return p.TurnID == e.TurnID }); index >= 0 {
			next.PendingInputs = slices.Clone(next.PendingInputs)
			next.PendingInputs[index].Content = e.Message
			next.PendingInputs[index].MultiContent = e.MultiContent
		}
	case *runtime.PendingUserMessageAcceptedEvent:
		if !slices.ContainsFunc(next.PendingInputs, func(p runtime.PendingInput) bool { return p.TurnID == e.TurnID }) {
			next.PendingInputs = append(slices.Clone(next.PendingInputs), runtime.PendingInput{TurnID: e.TurnID, Content: e.Message, MultiContent: e.MultiContent, SessionPosition: e.SessionPosition, InputOrigin: e.InputOrigin, SenderID: e.SenderID, SenderName: e.SenderName, InputMode: e.InputMode})
		}
	case *runtime.PendingUserMessagePromotedEvent:
		next.PendingInputs = slices.DeleteFunc(slices.Clone(next.PendingInputs), func(p runtime.PendingInput) bool { return p.TurnID == e.TurnID })
	case *runtime.PendingUserMessageCanceledEvent:
		next.PendingInputs = slices.DeleteFunc(slices.Clone(next.PendingInputs), func(p runtime.PendingInput) bool { return p.TurnID == e.TurnID })
	}
	next.Status.Pending = len(next.PendingInputs)
	a.presentation.Store(&next)
	return &next
}

func (s *appProjectionSink) OnError(err error) {
	if s.ctx.Err() == nil {
		s.app.sendEvent(s.ctx, runtime.Error(err.Error()))
	}
}

// The App view is detached from the runtime's mutable session. Commit positions
// make replay idempotent while avoiding transcript cloning on streamed tokens.
func (a *App) projectTranscript(event runtime.Event) {
	sess := a.Session()
	if sess == nil {
		return
	}
	switch e := event.(type) {
	case *runtime.SessionTitleEvent:
		sess.SetTitle(e.Title)
	case *runtime.TokenUsageEvent:
		if e.Usage != nil {
			if e.SessionID == "" || e.SessionID == sess.ID {
				sess.SetUsage(e.Usage.InputTokens, e.Usage.OutputTokens)
			}
			if usage := e.Usage.LastMessage; usage != nil {
				sess.AddMessageUsageRecord(e.AgentName, usage.Model, usage.Cost, &usage.Usage)
			}
		}
	case *runtime.MessageAddedEvent:
		if e.Message != nil && (e.SessionPosition < 0 || e.SessionPosition >= sess.ItemCount()) {
			message := *e.Message
			sess.AddMessage(&message)
		}
	case *runtime.UserMessageEvent:
		if e.SessionPosition < 0 || e.SessionPosition >= sess.ItemCount() {
			msg := session.UserMessage(e.Message, e.MultiContent...)
			msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode = e.InputOrigin, e.SenderID, e.SenderName, e.InputMode
			msg.TurnID = e.TurnID
			msg.Implicit = e.InputOrigin == session.InputOriginRuntime
			sess.AddMessage(msg)
		}
	case *runtime.PendingUserMessageAcceptedEvent:
		if e.SessionPosition >= sess.ItemCount() {
			msg := session.UserMessage(e.Message, e.MultiContent...)
			msg.TurnID, msg.Pending, msg.Accepted = e.TurnID, true, true
			msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode = e.InputOrigin, e.SenderID, e.SenderName, e.InputMode
			msg.Implicit = e.InputOrigin == session.InputOriginRuntime
			sess.AddMessage(msg)
		}
	case *runtime.PendingUserMessagePromotedEvent:
		sess.PromotePendingUserMessageByTurnID(e.TurnID)
	case *runtime.PendingUserMessageEditedEvent:
		if e.SessionID == sess.ID {
			sess.ReplacePendingUserMessagePayload(e.TurnID, e.Message, e.MultiContent)
		}
	case *runtime.PendingUserMessageCanceledEvent:
		sess.RemovePendingUserMessageByTurnID(e.TurnID)
	}
}

// EditSession sends intent to the canonical owner; callers never mutate the
// detached presentation snapshot to control execution.
func (a *App) EditSession(ctx context.Context, edit runtime.SessionEdit) error {
	state := a.state()
	if state.handle == nil {
		return state.operationError("edit")
	}
	snapshot, err := state.handle.Edit(ctx, edit)
	if err != nil {
		return err
	}
	switch edit.Kind {
	case runtime.SessionEditPendingMessage, runtime.SessionEditResume:
		// The canonical edited event updates the projection without a new bridge.
	case runtime.SessionEditMessage, runtime.SessionEditSummary, runtime.SessionEditTokens:
		a.startSessionEventBridge(ctx)
	default:
		a.installSessionView(ctx, state.handle, snapshot)
	}
	return nil
}

func (a *App) installSessionView(ctx context.Context, handle runtime.SessionHandle, snapshot *session.Session) {
	if snapshot == nil {
		return
	}
	a.projectionMu.Lock()
	a.stateMu.Lock()
	if a.currentState.handle != handle {
		a.stateMu.Unlock()
		a.projectionMu.Unlock()
		return
	}
	a.currentState.session = snapshot
	a.stateMu.Unlock()
	epoch := a.bridgeEpoch.Load()
	a.projectionMu.Unlock()
	a.sendBridgedEventFrom(ctx, "", &SessionViewEvent{Session: snapshot.Clone()}, true, snapshot.ID, epoch)
}
