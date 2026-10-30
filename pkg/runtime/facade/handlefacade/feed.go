package handlefacade

import (
	"context"
	"errors"
	"maps"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/runtime/facade"
)

// errFeedEnded reports that the session observation terminated before a turn settled.
var errFeedEnded = errors.New("session observation ended before turn settled")

// feed is the single session-level observation. It is the only reader of the
// runtime stream for turns and it keeps the interaction and queue registries
// current. It never blocks on consumers: all delivery goes through mailboxes.
type feed struct {
	cancel context.CancelFunc
	tr     *translator
	done   chan struct{}

	mu          sync.Mutex
	turns       map[string]*turnState
	outstanding map[string]pendingInteraction
	resolved    map[string]struct{}
	consumed    map[string]struct{}
	stopped     bool
	closing     bool
}

type pendingInteraction struct {
	kind          facade.InteractionKind
	elicitationID string
}

// turnState is one admitted turn's delivery and settlement.
type turnState struct {
	id      string
	box     *mailbox
	settled chan struct{}
	once    sync.Once
	outcome facade.Outcome
	err     error
}

func newTurnState(id string) *turnState {
	return &turnState{id: id, box: newMailbox(), settled: make(chan struct{})}
}

func (ts *turnState) settle(outcome facade.Outcome, err error) {
	ts.once.Do(func() {
		ts.outcome, ts.err = outcome, err
		ts.box.finish()
		close(ts.settled)
	})
}

// startFeed establishes the observation before any turn can be admitted, so
// every event of an admitted turn reaches the registry.
func startFeed(ctx context.Context, handle runtime.SessionHandle, sessionID string) (*feed, error) {
	observation, err := runtimeclient.ObserveTurnStartup(ctx, handle)
	if err != nil {
		return nil, err
	}
	// Cancellation belongs to the session, never to the caller that admitted the first turn.
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &feed{
		cancel:      cancel,
		tr:          newTranslator(sessionID),
		done:        make(chan struct{}),
		turns:       map[string]*turnState{},
		outstanding: map[string]pendingInteraction{},
		resolved:    map[string]struct{}{},
		consumed:    map[string]struct{}{},
	}
	for _, interaction := range observation.Primary().Interactions {
		f.outstanding[interaction.InteractionID] = pendingInteraction{kind: facade.InteractionKind(interaction.Kind), elicitationID: interaction.ElicitationID}
	}
	go f.run(fctx, observation)
	return f, nil
}

func (f *feed) run(ctx context.Context, observation runtime.Observation) {
	defer close(f.done)
	defer f.cancel()
	defer func() {
		if observation.Cancel != nil {
			observation.Cancel()
		}
	}()
	for _, env := range observation.Replay {
		f.handle(env)
	}
	events, errs := observation.Events, observation.Errors
	for {
		select {
		case <-ctx.Done():
			f.stop(facade.ErrSessionClosed)
			return
		case env, ok := <-events:
			if !ok {
				f.stop(errFeedEnded)
				return
			}
			if f.handle(env) {
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				f.stop(err)
				return
			}
		}
	}
}

// handle applies one envelope and reports whether the feed should now release
// its observation (a close was requested and no turn remains active).
func (f *feed) handle(env runtime.SessionEvent) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.track(env)
	for _, ev := range f.tr.translate(env) {
		if _, gap := ev.Payload.(facade.Gap); gap {
			for _, ts := range f.turns {
				ts.box.push(ev)
			}
			continue
		}
		ts := f.turns[ev.TurnID]
		if ev.TurnID == "" || ts == nil {
			continue
		}
		ts.box.push(ev)
		if settled, ok := ev.Payload.(facade.TurnSettled); ok {
			delete(f.turns, ev.TurnID)
			ts.settle(settled.Outcome, nil)
		}
	}
	return f.closing && len(f.turns) == 0
}

// track keeps the outstanding-interaction and consumed-ticket registries current.
func (f *feed) track(env runtime.SessionEvent) {
	switch event := env.Event.(type) {
	case *runtime.ToolCallConfirmationEvent:
		f.addOutstanding(env.InteractionID, pendingInteraction{kind: facade.InteractionConfirmation})
	case *runtime.MaxIterationsReachedEvent:
		f.addOutstanding(env.InteractionID, pendingInteraction{kind: facade.InteractionMaxIterations})
	case *runtime.ElicitationRequestEvent:
		f.addOutstanding(env.InteractionID, pendingInteraction{kind: facade.InteractionElicitation, elicitationID: event.ElicitationID})
	case *runtime.InteractionResolvedEvent:
		delete(f.outstanding, event.InteractionID)
		if event.InteractionID != "" {
			f.resolved[event.InteractionID] = struct{}{}
		}
	case *runtime.PendingUserMessagePromotedEvent:
		f.consumed[event.TurnID] = struct{}{}
	}
}

func (f *feed) addOutstanding(id string, interaction pendingInteraction) {
	if id != "" {
		f.outstanding[id] = interaction
	}
}

// register admits a turn subscriber before submission so no event can be missed.
func (f *feed) register(id string) (*turnState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.closing {
		return nil, facade.ErrSessionClosed
	}
	if _, exists := f.turns[id]; exists {
		return nil, errors.New("duplicate turn identity")
	}
	ts := newTurnState(id)
	f.turns[id] = ts
	return ts, nil
}

// unregister drops a subscriber whose submission was rejected.
func (f *feed) unregister(ts *turnState) {
	f.mu.Lock()
	if f.turns[ts.id] == ts {
		delete(f.turns, ts.id)
	}
	f.mu.Unlock()
	ts.settle(facade.Outcome{}, errors.New("turn was not admitted"))
}

func (f *feed) stop(err error) {
	f.mu.Lock()
	f.stopped = true
	turns := f.turns
	f.turns = map[string]*turnState{}
	f.mu.Unlock()
	for _, ts := range turns {
		ts.settle(facade.Outcome{}, err)
	}
}

// requestClose releases the observation once no turn is active.
func (f *feed) requestClose() {
	f.mu.Lock()
	f.closing = true
	idle := len(f.turns) == 0
	f.mu.Unlock()
	if idle {
		f.cancel()
	}
}

func (f *feed) isConsumed(ticketID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.consumed[ticketID]
	return ok
}

func (f *feed) outstandingSnapshot() map[string]pendingInteraction {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]pendingInteraction, len(f.outstanding))
	maps.Copy(out, f.outstanding)
	return out
}

func (f *feed) wasResolved(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.resolved[id]
	return ok
}
