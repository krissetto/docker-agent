package handlefacade

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/subagent"
)

// translator maps runtime session events onto facade events. It carries the
// per-stream state needed to normalize settlement (stop reason, error message)
// and subagent status deduplication. One translator serves one ordered stream.
type translator struct {
	sessionID  string
	reasons    map[string]string
	errors     map[string]string
	nodeStates map[subagent.NodeID]string
}

func newTranslator(sessionID string) *translator {
	return &translator{
		sessionID:  sessionID,
		reasons:    map[string]string{},
		errors:     map[string]string{},
		nodeStates: map[subagent.NodeID]string{},
	}
}

// translate maps one envelope to facade events. Every event produced from one
// envelope carries that envelope's Seq. Turn-scoped settlement is routed by the
// caller, so TurnSettled is returned here but never fanned out by Epoch.
func (t *translator) translate(env runtime.SessionEvent) []facade.Event {
	if env.Gap {
		first := env.FirstAvailable
		seq := uint64(0)
		if first > 0 {
			seq = first - 1
		}
		return []facade.Event{t.envelope(env, seq, facade.Gap{FirstAvailable: first})}
	}
	if env.Event == nil {
		return nil
	}
	var payload facade.Payload
	switch event := env.Event.(type) {
	case *runtime.StreamStartedEvent:
		payload = facade.TurnStarted{}
	case *runtime.StreamStoppedEvent:
		t.reasons[env.TurnID] = event.Reason
		payload = facade.Passthrough{Event: env.Event}
	case *runtime.ErrorEvent:
		if env.TurnID != "" {
			t.errors[env.TurnID] = event.Error
		}
		payload = facade.Passthrough{Event: env.Event}
	case *runtime.TurnSettledEvent:
		payload = facade.TurnSettled{Outcome: t.outcome(env.TurnID, event.Outcome)}
	case *runtime.ToolCallConfirmationEvent:
		payload = facade.InteractionRequested{
			InteractionID: env.InteractionID,
			Kind:          facade.InteractionConfirmation,
			Confirmation: &facade.Confirmation{
				ToolCall: event.ToolCall,
				Tool:     event.ToolDefinition,
				Metadata: event.Metadata,
			},
		}
	case *runtime.MaxIterationsReachedEvent:
		payload = facade.InteractionRequested{InteractionID: env.InteractionID, Kind: facade.InteractionMaxIterations, MaxIterations: event.MaxIterations}
	case *runtime.ElicitationRequestEvent:
		payload = facade.InteractionRequested{
			InteractionID: env.InteractionID,
			Kind:          facade.InteractionElicitation,
			Elicitation: &facade.ElicitationRequest{
				ElicitationID: event.ElicitationID,
				Mode:          event.Mode,
				Message:       event.Message,
				URL:           event.URL,
				Schema:        event.Schema,
				Meta:          event.Meta,
			},
		}
	case *runtime.InteractionResolvedEvent:
		payload = facade.InteractionResolved{InteractionID: event.InteractionID, Resolution: facade.InteractionResolution(event.Reason)}
	case *runtime.PendingUserMessageAcceptedEvent:
		payload = facade.QueueAccepted{Ticket: facade.Ticket{ID: event.TurnID}}
	case *runtime.PendingUserMessageCanceledEvent:
		payload = facade.QueueWithdrawn{Ticket: facade.Ticket{ID: event.TurnID}}
	case *runtime.PendingUserMessagePromotedEvent:
		payload = facade.QueueConsumed{Ticket: facade.Ticket{ID: event.TurnID}}
	case *runtime.SubagentCreatedEvent:
		payload = facade.SubagentStatus{
			ChildSessionID:  event.ChildSessionID,
			ParentSessionID: event.ParentSessionID,
			State:           string(subagent.NodeStarting),
		}
	case *runtime.SubagentTreeEvent:
		return t.treeStatuses(env, event)
	default:
		payload = facade.Passthrough{Event: env.Event}
	}
	return []facade.Event{t.envelope(env, env.Sequence, payload)}
}

func (t *translator) envelope(env runtime.SessionEvent, seq uint64, payload facade.Payload) facade.Event {
	return facade.Event{SessionID: env.SessionID, TurnID: env.TurnID, Seq: seq, Epoch: env.Epoch, MessageID: messageIDOf(env.Event), Payload: payload}
}

// messageIDOf returns the message identity the runtime provided on a chunk.
// Other events, and chunks without an ID, have none; it is never synthesized.
func messageIDOf(event runtime.Event) string {
	switch event := event.(type) {
	case *runtime.AgentChoiceEvent:
		return event.MessageID
	case *runtime.AgentChoiceReasoningEvent:
		return event.MessageID
	}
	return ""
}

func (t *translator) outcome(turnID string, outcome runtime.TurnOutcome) facade.Outcome {
	result := facade.Outcome{Reason: t.reasons[turnID]}
	switch outcome {
	case runtime.TurnCanceled:
		result.Kind = facade.OutcomeCanceled
	case runtime.TurnFailed:
		result.Kind = facade.OutcomeFailed
		result.Message = t.errors[turnID]
	default:
		result.Kind = facade.OutcomeCompleted
	}
	return result
}

// treeStatuses emits one SubagentStatus per node whose state changed since this
// stream last saw it. Nodes are flattened with their parent session resolved.
func (t *translator) treeStatuses(env runtime.SessionEvent, event *runtime.SubagentTreeEvent) []facade.Event {
	var out []facade.Event
	var walk func(nodes []subagent.NodeSnapshot, parent string)
	walk = func(nodes []subagent.NodeSnapshot, parent string) {
		for _, n := range nodes {
			// The root node is the session itself, not a subagent, so it has no child session;
			// its children are parented to the observed root, not to its empty session ID.
			next := parent
			if n.Node.SessionID != "" {
				next = n.Node.SessionID
			}
			if n.Node.SessionID != "" {
				if last, ok := t.nodeStates[n.Node.ID]; !ok || last != string(n.Node.State) {
					t.nodeStates[n.Node.ID] = string(n.Node.State)
					out = append(out, t.envelope(env, env.Sequence, facade.SubagentStatus{
						ChildSessionID:  n.Node.SessionID,
						ParentSessionID: parent,
						Agent:           n.Node.Agent,
						State:           string(n.Node.State),
					}))
				}
			}
			walk(n.Children, next)
		}
	}
	walk(event.Snapshot.Nodes, t.sessionID)
	return out
}
