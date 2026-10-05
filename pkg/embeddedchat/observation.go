package embeddedchat

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
)

// Observe attaches a snapshot-plus-tail view of the current conversation.
// Cancelling ctx or closing the Session detaches the view, not the running turn.
// Only one observation may be active per Session. Send retains its separate
// exact-turn cancellation and drain lifetime; interactions are delivered once
// across both streams. A Snapshot event is a complete rebaseline barrier.
func (s *Session) Observe(ctx context.Context) (<-chan Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.handle == nil {
		return nil, ErrNotInitialized
	}
	if s.transitioning || s.observerCancel != nil {
		return nil, ErrRunActive
	}
	child, cancel := context.WithCancel(ctx)
	out := make(chan Event, eventBufferSize(s.cfg.EventBuffer))
	done := make(chan struct{})
	sink := &observationSink{session: s, handle: s.handle, generation: s.generation, ctx: child, cancel: cancel, out: out}
	attachment, err := runtimeclient.Attach(child, s.handle, sink)
	if err != nil {
		cancel()
		return nil, err
	}
	s.observerCancel, s.observerDone = cancel, done
	go func() {
		<-child.Done()
		attachment.Detach()
		s.mu.Lock()
		if s.observerDone == done {
			s.observerCancel, s.observerDone = nil, nil
		}
		s.mu.Unlock()
		close(out)
		close(done)
	}()
	return out, nil
}

type observationSink struct {
	session    *Session
	handle     runtime.SessionHandle
	generation uint64
	ctx        context.Context //nolint:containedctx // bounded by Observe's attachment lifetime
	cancel     context.CancelFunc
	out        chan<- Event
}

func (o *observationSink) emit(event Event) bool {
	select {
	case <-o.ctx.Done():
		return false
	default:
	}
	select {
	case o.out <- event:
		return true
	case <-o.ctx.Done():
		return false
	}
}

func (o *observationSink) Reset(snapshot runtime.SessionSnapshot) {
	s := o.session
	s.mu.Lock()
	if s.closed || s.transitioning || s.generation != o.generation || s.handle != o.handle || o.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	// Reconcile rather than forgetting delivered tokens: gap recovery must not
	// run the same synchronous interaction handler twice.
	pending := make(map[string]runtime.SessionHandle)
	seen := make(map[string]bool)
	s.pendingConfirmation, s.pendingHandle = "", nil
	for _, interaction := range snapshot.Interactions {
		if s.seenInteractions[interaction.InteractionID] {
			seen[interaction.InteractionID] = true
			pending[interaction.InteractionID] = o.handle
			if interaction.Kind == runtime.InteractionConfirmation {
				s.pendingConfirmation, s.pendingHandle = interaction.InteractionID, o.handle
			}
		}
	}
	s.pendingInteractions, s.seenInteractions = pending, seen
	s.mu.Unlock()
	if !o.emit(Event{Snapshot: &snapshot}) {
		return
	}
	for _, interaction := range snapshot.Interactions {
		if !s.forwardInteraction(o.ctx, o.handle, interaction, o.emit) {
			o.cancel()
			return
		}
	}
}

func (o *observationSink) Apply(envelope runtime.SessionEvent) {
	s := o.session
	s.mu.Lock()
	valid := !s.closed && !s.transitioning && s.generation == o.generation && s.handle == o.handle && o.ctx.Err() == nil
	if resolved, ok := envelope.Event.(*runtime.InteractionResolvedEvent); ok && valid {
		delete(s.pendingInteractions, resolved.InteractionID)
		if s.pendingConfirmation == resolved.InteractionID {
			s.pendingConfirmation, s.pendingHandle = "", nil
		}
	}
	s.mu.Unlock()
	if !valid {
		return
	}
	interaction := runtime.InteractionSnapshot{SessionID: envelope.SessionID, InteractionID: envelope.InteractionID, Event: envelope.Event}
	switch e := envelope.Event.(type) {
	case *runtime.ToolCallConfirmationEvent:
		interaction.Kind = runtime.InteractionConfirmation
	case *runtime.ElicitationRequestEvent:
		interaction.Kind, interaction.ElicitationID = runtime.InteractionElicitation, e.ElicitationID
	case *runtime.MaxIterationsReachedEvent:
		interaction.Kind = runtime.InteractionMaxIterations
	case *runtime.StreamStoppedEvent:
		o.emit(Event{RuntimeEvent: e, Done: true})
		return
	case *runtime.ErrorEvent:
		o.emit(Event{RuntimeEvent: e, Err: errors.New(e.Error)})
		return
	default:
		if event, ok := TranslateRuntimeEvent(envelope.Event); ok {
			o.emit(event)
		} else {
			o.emit(Event{RuntimeEvent: envelope.Event})
		}
		return
	}
	if !s.forwardInteraction(o.ctx, o.handle, interaction, o.emit) {
		o.cancel()
	}
}

func (o *observationSink) OnError(err error) {
	o.emit(Event{Err: err})
	o.cancel()
}

// Transport failures are explicit without marking owner execution as finished.
func (o *observationSink) OnConnectionState(connected bool, err error) {
	if !connected && err != nil {
		o.emit(Event{Err: err})
	}
}
