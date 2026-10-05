// Package lifecycle reduces runtime session events into UI-neutral lifecycle state.
package lifecycle

import (
	"slices"

	"github.com/docker/docker-agent/pkg/runtime"
)

type ActionKind uint8

const (
	TurnStarted ActionKind = iota + 1
	TurnSettled
	PendingAccepted
	PendingPromoted
	CancelSettled
	CompactionBoundary
)

type Action struct {
	Kind      ActionKind
	TurnID    string
	AgentName string
	Own       bool
	Foreign   bool
	Phase     string
	Boundary  bool
}

type Stream struct {
	SessionID string
	AgentName string
}

type State struct {
	SessionID string
	TurnID    string
	Status    runtime.SessionState
	Streams   []Stream
	Pending   []string
}

func FromSnapshot(snapshot runtime.SessionSnapshot) State {
	pending := make([]string, 0, len(snapshot.PendingInputs))
	for _, input := range snapshot.PendingInputs {
		pending = append(pending, input.TurnID)
	}
	return State{SessionID: snapshot.Status.SessionID, TurnID: snapshot.Status.TurnID, Status: snapshot.Status.State, Pending: pending}
}

func (s State) ApplySession(event runtime.SessionEvent) (State, []Action) {
	if s.SessionID == "" {
		s.SessionID = event.SessionID
	}
	return s.apply(event.Event, event.TurnID)
}

func (s State) Apply(event runtime.Event) (State, []Action) { return s.apply(event, "") }

func (s State) apply(event runtime.Event, envelopeTurnID string) (State, []Action) {
	s.Streams = slices.Clone(s.Streams)
	s.Pending = slices.Clone(s.Pending)
	switch event := event.(type) {
	case *runtime.StreamStartedEvent:
		if envelopeTurnID != "" && (s.SessionID == "" || event.SessionID == s.SessionID) {
			s.TurnID = envelopeTurnID
		}
		s.Streams = append(s.Streams, Stream{SessionID: event.SessionID, AgentName: event.AgentName})
		s.Status = runtime.SessionStateRunning
		return s, []Action{{Kind: TurnStarted, TurnID: envelopeTurnID, AgentName: event.AgentName}}
	case *runtime.StreamStoppedEvent:
		if len(s.Streams) > 0 {
			s.Streams = s.Streams[:len(s.Streams)-1]
		}
		if len(s.Streams) == 0 && s.TurnID == "" {
			s.Status = runtime.SessionStateSettled
		}
		if s.TurnID != "" {
			return s, nil
		}
		kind := TurnSettled
		if event.Reason == "cancelled" || event.Reason == "canceled" {
			kind = CancelSettled
		}
		return s, []Action{{Kind: kind, TurnID: envelopeTurnID, AgentName: event.AgentName}}
	case *runtime.TurnSettledEvent:
		if event.SessionID != s.SessionID || event.TurnID == "" || event.TurnID != s.TurnID {
			return s, nil
		}
		s.TurnID = ""
		s.Status = runtime.SessionStateSettled
		kind := TurnSettled
		if event.Outcome == runtime.TurnCanceled {
			kind = CancelSettled
		}
		return s, []Action{{Kind: kind, TurnID: event.TurnID, AgentName: event.AgentName}}
	case *runtime.PendingUserMessageAcceptedEvent:
		if !slices.Contains(s.Pending, event.TurnID) {
			s.Pending = append(s.Pending, event.TurnID)
		}
		return s, []Action{{Kind: PendingAccepted, TurnID: event.TurnID}}
	case *runtime.PendingUserMessagePromotedEvent:
		s.Pending = slices.DeleteFunc(s.Pending, func(id string) bool { return id == event.TurnID })
		return s, []Action{{Kind: PendingPromoted, TurnID: event.TurnID}}
	case *runtime.SessionCompactionEvent:
		foreign := s.SessionID != "" && event.SessionID != "" && event.SessionID != s.SessionID
		boundary := event.Status == "started" || event.Status == "completed"
		return s, []Action{{Kind: CompactionBoundary, Own: !foreign, Foreign: foreign, Phase: event.Status, Boundary: boundary}}
	default:
		return s, nil
	}
}

func (s State) Depth() int { return len(s.Streams) }
