package handlefacade

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
)

type observerCap struct{ s *sessionImpl }

// Observe replays from the cursor (or snapshots when nil) and streams events.
// A gap is reported as a Gap event; the consumer must then observe again without a cursor.
func (c observerCap) Observe(ctx context.Context, since *facade.Cursor) (facade.Observation, error) {
	options := runtime.ObserveOptions{}
	if since != nil {
		sequence := since.Sequence
		options.Since = &sequence
		options.SinceEpoch = since.Epoch
	}
	observation, err := c.s.handle.Observe(ctx, options)
	if err != nil {
		return facade.Observation{}, err
	}
	initial := observation.Primary()
	tr := newTranslator(c.s.handle.ID())
	box := newMailbox()
	for _, env := range observation.Replay {
		for _, ev := range tr.translate(env) {
			box.push(ev)
		}
	}
	state := facade.ObservedState{
		Epoch:        initial.Epoch,
		Cursor:       initial.Cursor,
		Status:       mapStatus(initial.Status),
		Interactions: mapInteractions(initial.Interactions),
	}
	stream := newStream(ctx, observation, func(env runtime.SessionEvent) []facade.Event { return tr.translate(env) }, box)
	return facade.Observation{State: state, Events: box.out, Close: stream.close}, nil
}

// stream pumps one runtime observation into a mailbox until it ends. Cancelling
// ctx or calling close detaches the observation; neither affects session work.
type stream struct {
	observation runtime.Observation
	once        sync.Once
	stopCtx     func() bool
}

func newStream(ctx context.Context, observation runtime.Observation, translate func(runtime.SessionEvent) []facade.Event, box *mailbox) *stream {
	st := &stream{observation: observation}
	st.stopCtx = context.AfterFunc(ctx, st.close)
	go st.pump(translate, box)
	return st
}

func (st *stream) pump(translate func(runtime.SessionEvent) []facade.Event, box *mailbox) {
	defer box.finish()
	events, errs := st.observation.Events, st.observation.Errors
	for {
		select {
		case env, ok := <-events:
			if !ok {
				return
			}
			for _, ev := range translate(env) {
				box.push(ev)
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			st.close()
		}
	}
}

func (st *stream) close() {
	st.once.Do(func() {
		if st.stopCtx != nil {
			st.stopCtx()
		}
		if st.observation.Cancel != nil {
			st.observation.Cancel()
		}
	})
}

type backgroundCap struct{ s *sessionImpl }

// Subscribe streams out-of-turn descendant usage and subagent status, derived from
// the ordered tree observation. Nothing else is guaranteed on this stream.
func (c backgroundCap) Subscribe(ctx context.Context) (facade.Subscription, error) {
	return SubscribeBackground(ctx, c.s.handle)
}

// SubscribeBackground borrows a resolved handle without opening the view again.
func SubscribeBackground(ctx context.Context, handle runtime.SessionHandle) (facade.Subscription, error) {
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{Tree: true, OrderedTree: true})
	if err != nil {
		return facade.Subscription{}, err
	}
	root := handle.ID()
	tr := newTranslator(root)
	box := newMailbox()
	seed := func(snapshot *runtime.SessionSnapshot) {
		if snapshot == nil || snapshot.Session == nil {
			return
		}
		if tree := snapshot.Session.GetSubagentTree(); tree != nil {
			for _, ev := range tr.translate(runtime.SessionEvent{SessionID: root, Event: &runtime.SubagentTreeEvent{Snapshot: *tree}}) {
				box.push(ev)
			}
		}
	}
	for i := range observation.Initial {
		seed(&observation.Initial[i])
	}
	emit := func(env runtime.SessionEvent) []facade.Event {
		if env.Gap {
			return tr.translate(env)
		}
		if usage, ok := env.Event.(*runtime.TokenUsageEvent); ok {
			if env.SessionID == root || usage.SessionID != env.SessionID {
				return nil
			}
			return []facade.Event{{SessionID: env.SessionID, Seq: env.Sequence, Epoch: env.Epoch, Payload: facade.Passthrough{Event: env.Event}}}
		}
		switch env.Event.(type) {
		case *runtime.SubagentCreatedEvent:
			return tr.translate(env)
		case *runtime.SubagentTreeEvent:
			if env.SessionID == root {
				return tr.translate(env)
			}
		}
		return nil
	}
	for _, env := range observation.Replay {
		for _, ev := range emit(env) {
			box.push(ev)
		}
	}
	st := &stream{observation: observation}
	st.stopCtx = context.AfterFunc(ctx, st.close)
	go pumpTree(ctx, st, observation, seed, func(env runtime.SessionEvent) {
		for _, ev := range emit(env) {
			box.push(ev)
		}
	}, box)
	return facade.Subscription{Events: box.out, Close: st.close}, nil
}

// pumpTree drains tree updates, live events and added sessions until the observation ends.
func pumpTree(ctx context.Context, st *stream, observation runtime.Observation, seed func(*runtime.SessionSnapshot), deliver func(runtime.SessionEvent), box *mailbox) {
	defer box.finish()
	defer st.close()
	added, events, updates, errs := observation.SessionsAdded, observation.Events, observation.TreeUpdates, observation.Errors
	for added != nil || events != nil || updates != nil {
		select {
		case <-ctx.Done():
			return
		case snapshot, ok := <-added:
			if !ok {
				added = nil
				continue
			}
			seed(&snapshot)
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if update.Snapshot != nil {
				seed(update.Snapshot)
			}
			for _, env := range update.Replay {
				deliver(env)
			}
			if update.Event != nil {
				deliver(*update.Event)
			}
		case env, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			deliver(env)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				return
			}
		}
	}
}
