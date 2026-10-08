package lifecycle

import (
	"fmt"
	"slices"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Projection is the immutable shared presentation head, without per-delta transcript copies.
type Projection struct {
	Lifecycle     State
	Status        runtime.SessionStatus
	PendingInputs []runtime.PendingInput
	Interactions  []runtime.InteractionSnapshot
	Connection    ConnectionState
}

// ConnectionState is the observer's transport state, never the session's
// execution state: a disconnected view's accepted work keeps running.
type ConnectionState uint32

const (
	ConnectionUnknown ConnectionState = iota
	ConnectionConnecting
	ConnectionConnected
	ConnectionReconnecting
	ConnectionDisconnected
)

func (s ConnectionState) String() string {
	switch s {
	case ConnectionConnecting:
		return "connecting"
	case ConnectionConnected:
		return "connected"
	case ConnectionReconnecting:
		return "reconnecting"
	case ConnectionDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// InteractionKey identifies prompts independently of transport payload identity.
type InteractionKey struct{ SessionID, InteractionID string }

func InteractionIdentity(event any) InteractionKey {
	switch e := event.(type) {
	case *runtime.ToolCallConfirmationEvent:
		return InteractionKey{e.SessionID, e.RequestID}
	case *runtime.MaxIterationsReachedEvent:
		return InteractionKey{e.SessionID, e.RequestID}
	case *runtime.ElicitationRequestEvent:
		return InteractionKey{e.SessionID, e.RequestID}
	}
	return InteractionKey{}
}

// RecoveryUncertain reports accepted inputs without durable terminal evidence.
func (p *Projection) RecoveryUncertain() bool {
	return p != nil && p.Status.InterruptedTurns > 0
}

// InterruptedTurnsNotice is the shared client wording for RecoveryUncertain.
func InterruptedTurnsNotice(n int) string {
	inputs, records := "accepted input has", "record"
	if n != 1 {
		inputs, records = "accepted inputs have", "records"
	}
	return fmt.Sprintf("Recovery uncertain: %d %s no terminal outcome %s. Review the transcript before continuing.", n, inputs, records)
}

func (p *Projection) HasInteraction(key InteractionKey) bool {
	if p == nil {
		return false
	}
	return slices.ContainsFunc(p.Interactions, func(i runtime.InteractionSnapshot) bool {
		return InteractionSnapshotKey(i) == key
	})
}

func InteractionSnapshotKey(i runtime.InteractionSnapshot) InteractionKey {
	key := InteractionKey{i.SessionID, i.InteractionID}
	if key.InteractionID == "" {
		return InteractionIdentity(i.Event)
	}
	return key
}

// IsUserInput deliberately leaves untyped and unknown origins unprivileged and visible.
func IsUserInput(origin session.InputOrigin) bool {
	return origin != session.InputOriginAgent && origin != session.InputOriginRuntime
}

func VisibleTranscriptMessage(message *session.Message) bool {
	return message != nil && !message.Implicit && !message.Pending
}

func InputSender(name, id string) string {
	id = ShortInputSenderID(id)
	if name == "" {
		return id
	}
	if id == "" {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, id)
}

func ShortInputSenderID(id string) string {
	return subagent.ShortID(id)
}

func RuntimeInputNotice(name, id string) string {
	if name == "" && id == "" {
		return "Runtime update received"
	}
	if name == "" {
		name = "subagent"
	}
	return InputSender(name, id) + " has replied"
}

// InputReplay reconciles consumed inputs with a snapshot without comparing their bodies.
// Pending snapshot entries are not consumed and must still render on promotion.
type InputReplay struct {
	turns     map[string]bool
	positions map[int]bool
}

func (r *InputReplay) Reset(sess *session.Session) {
	*r = InputReplay{turns: make(map[string]bool), positions: make(map[int]bool)}
	if sess == nil {
		return
	}
	for pos, item := range sess.ItemsSnapshot() {
		if item.Message == nil || item.Message.Pending {
			continue
		}
		r.positions[pos] = true
		if item.Message.TurnID != "" {
			r.turns[item.Message.TurnID] = true
		}
	}
}

// Contains reports consumed or withdrawn identity without changing replay state.
func (r *InputReplay) Contains(turnID string, position int) bool {
	if turnID != "" {
		return r.turns[turnID]
	}
	return position >= 0 && r.positions[position]
}

func (r *InputReplay) Cancel(turnID string, position int) {
	if r.turns == nil {
		r.Reset(nil)
	}
	r.Withdraw(position)
	if turnID != "" {
		r.turns[turnID] = true
	}
}

func (r *InputReplay) Consume(turnID string, position int) bool {
	if r.turns == nil {
		r.Reset(nil)
	}
	if turnID != "" {
		if r.turns[turnID] {
			return false
		}
		r.turns[turnID] = true
	} else if position >= 0 && r.positions[position] {
		return false
	}
	if position >= 0 {
		r.positions[position] = true
	}
	return true
}

func (r *InputReplay) Withdraw(position int) {
	if position < 0 {
		return
	}
	shifted := make(map[int]bool, len(r.positions))
	for pos := range r.positions {
		if pos < position {
			shifted[pos] = true
		} else if pos > position {
			shifted[pos-1] = true
		}
	}
	r.positions = shifted
}
