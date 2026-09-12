package runtime

import (
	"encoding/json"
	"strings"
	"sync"
)

const defaultSessionEventReplayCapacity = 1024

// SequencedSessionEvent is a replayable session event. Gap marks that events
// after the requested cursor were evicted and the consumer must resnapshot.
type SequencedSessionEvent struct {
	Sequence       uint64
	RequestID      string
	InteractionID  string
	Event          Event
	Gap            bool
	FirstAvailable uint64
}

type retainedSessionEvent struct {
	sequence      uint64
	requestID     string
	interactionID string
	generation    uint64
	event         Event
	bytes         int
}

type sessionEventHub struct {
	mu         sync.Mutex
	subs       map[string]map[*sessionEventSubscriber]struct{}
	seqSubs    map[string]map[*sequencedSessionEventSubscriber]struct{}
	inflight   map[string]*inflightAssistant
	liveRuns   map[string]int
	liveAgent  map[string]string
	nextSeq    map[string]uint64
	requestID  map[string]string
	generation map[string]uint64
	terminal   map[string]bool
	deleting   map[string]bool
	closed     map[string]bool
	replay     map[string][]retainedSessionEvent
	capacity   int
	maxBytes   int
	bytes      map[string]int
}

type sessionEventSubscriber struct {
	out    chan Event
	limit  int
	closed bool
}

type sequencedSessionEventSubscriber struct {
	out    chan SequencedSessionEvent
	limit  int
	closed bool
}

type inflightAssistant struct {
	agentName string
	content   strings.Builder
	reasoning strings.Builder
}

func newSessionEventHubWithLimits(capacity, maxBytes int) *sessionEventHub {
	if capacity < 0 || maxBytes < 0 {
		panic("session replay limits cannot be negative")
	}
	return &sessionEventHub{
		subs:       map[string]map[*sessionEventSubscriber]struct{}{},
		seqSubs:    map[string]map[*sequencedSessionEventSubscriber]struct{}{},
		inflight:   map[string]*inflightAssistant{},
		liveRuns:   map[string]int{},
		liveAgent:  map[string]string{},
		nextSeq:    map[string]uint64{},
		requestID:  map[string]string{},
		generation: map[string]uint64{},
		terminal:   map[string]bool{},
		deleting:   map[string]bool{},
		closed:     map[string]bool{},
		replay:     map[string][]retainedSessionEvent{},
		capacity:   capacity,
		maxBytes:   maxBytes,
		bytes:      map[string]int{},
	}
}

// Subscribe is the compatibility event surface. Its channel is bounded; a
// slow consumer is disconnected rather than retaining an unbounded queue.
func (h *sessionEventHub) Subscribe(sessionID string, buffer int) (seed []Event, _ <-chan Event, cancel func()) {
	if buffer < 1 {
		buffer = 1
	}
	sub := &sessionEventSubscriber{out: make(chan Event, buffer+1), limit: buffer}
	h.mu.Lock()
	seed = h.liveSeedLocked(sessionID)
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[*sessionEventSubscriber]struct{}{}
	}
	h.subs[sessionID][sub] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return seed, sub.out, func() {
		once.Do(func() {
			func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.removeSubscriberLocked(sessionID, sub)
			}()
		})
	}
}

// SubscribeSequenced atomically registers a bounded subscriber and returns
// retained events after since. A nil cursor starts from current live state.
func (h *sessionEventHub) SubscribeSequenced(sessionID string, since *uint64, buffer int) (seed []SequencedSessionEvent, events <-chan SequencedSessionEvent, cancel func(), cursor uint64) {
	if buffer < 1 {
		buffer = 1
	}
	sub := &sequencedSessionEventSubscriber{out: make(chan SequencedSessionEvent, buffer+1), limit: buffer}
	h.mu.Lock()
	cursor = h.nextSeq[sessionID]
	if since == nil {
		for _, event := range h.liveSeedLocked(sessionID) {
			seed = append(seed, SequencedSessionEvent{Event: event})
		}
	} else {
		seed = h.replayLocked(sessionID, *since)
	}
	if h.seqSubs[sessionID] == nil {
		h.seqSubs[sessionID] = map[*sequencedSessionEventSubscriber]struct{}{}
	}
	h.seqSubs[sessionID][sub] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	cancel = func() {
		once.Do(func() {
			func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.removeSequencedSubscriberLocked(sessionID, sub)
			}()
		})
	}
	return seed, sub.out, cancel, cursor
}

func (h *sessionEventHub) PublishForRequest(sessionID, requestID string, event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.requestID[sessionID]
	h.requestID[sessionID] = requestID
	h.publishLocked(sessionID, event)
	h.requestID[sessionID] = previous
}

func (h *sessionEventHub) Publish(sessionID string, event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishLocked(sessionID, event)
}

func (h *sessionEventHub) publishLocked(sessionID string, event Event) {
	if h.closed[sessionID] {
		return
	}
	terminalEvent := false
	if stopped, ok := event.(*StreamStoppedEvent); ok {
		terminalEvent = h.liveRuns[sessionID] <= 1
		if terminalEvent && h.terminal[sessionID] {
			return
		}
		if terminalEvent && h.deleting[sessionID] {
			event = StreamStopped(sessionID, stopped.AgentName, "deleted")
		}
		if terminalEvent {
			h.terminal[sessionID] = true
		}
	}
	h.trackInflightLocked(sessionID, event)
	interactionID := interactionEventID(event)
	sequence := h.nextSeq[sessionID] + 1
	h.nextSeq[sessionID] = sequence
	h.appendReplayLocked(sessionID, retainedSessionEvent{sequence: sequence, requestID: h.requestID[sessionID], interactionID: interactionID, generation: h.generation[sessionID], event: event, bytes: estimateEventBytes(event)})
	for sub := range h.subs[sessionID] {
		if !terminalEvent && len(sub.out) >= sub.limit {
			h.removeSubscriberLocked(sessionID, sub)
			continue
		}
		select {
		case sub.out <- event:
		default:
			h.removeSubscriberLocked(sessionID, sub)
		}
	}
	for sub := range h.seqSubs[sessionID] {
		if !terminalEvent && len(sub.out) >= sub.limit {
			h.removeSequencedSubscriberLocked(sessionID, sub)
			continue
		}
		select {
		case sub.out <- SequencedSessionEvent{Sequence: sequence, RequestID: h.requestID[sessionID], InteractionID: interactionID, Event: event}:
		default:
			h.removeSequencedSubscriberLocked(sessionID, sub)
		}
	}
}

func (h *sessionEventHub) liveSeedLocked(sessionID string) []Event {
	var seed []Event
	if h.liveRuns[sessionID] > 0 {
		seed = append(seed, StreamStarted(sessionID, h.liveAgent[sessionID]))
	}
	if st := h.inflight[sessionID]; st != nil {
		if st.reasoning.Len() > 0 {
			seed = append(seed, AgentChoiceReasoning(st.agentName, sessionID, st.reasoning.String()))
		}
		if st.content.Len() > 0 {
			seed = append(seed, AgentChoice(st.agentName, sessionID, st.content.String()))
		}
	}
	return seed
}

func estimateEventBytes(event Event) int {
	data, err := json.Marshal(event)
	if err != nil {
		return 256
	}
	return len(data)
}

func (h *sessionEventHub) appendReplayLocked(sessionID string, event retainedSessionEvent) {
	replay := append(h.replay[sessionID], event)
	h.bytes[sessionID] += event.bytes
	for len(replay) > 0 && (len(replay) > h.capacity || h.bytes[sessionID] > h.maxBytes || h.capacity == 0 || h.maxBytes == 0) {
		h.bytes[sessionID] -= replay[0].bytes
		replay[0].event = nil
		replay = replay[1:]
	}
	h.replay[sessionID] = replay
}

func (h *sessionEventHub) replayLocked(sessionID string, since uint64) []SequencedSessionEvent {
	replay := h.replay[sessionID]
	if len(replay) == 0 {
		if since < h.nextSeq[sessionID] {
			return []SequencedSessionEvent{{Gap: true, FirstAvailable: h.nextSeq[sessionID] + 1}}
		}
		return nil
	}
	out := make([]SequencedSessionEvent, 0, len(replay)+1)
	if since > h.nextSeq[sessionID] || replay[0].sequence > since+1 {
		out = append(out, SequencedSessionEvent{Gap: true, FirstAvailable: replay[0].sequence})
	}
	for _, retained := range replay {
		if retained.sequence > since {
			out = append(out, SequencedSessionEvent{Sequence: retained.sequence, RequestID: retained.requestID, InteractionID: retained.interactionID, Event: retained.event})
		}
	}
	return out
}

func (h *sessionEventHub) SetRequest(sessionID, requestID string, generation uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if requestID == "" {
		delete(h.requestID, sessionID)
		delete(h.generation, sessionID)
	} else {
		h.requestID[sessionID] = requestID
		h.generation[sessionID] = generation
	}
}

func interactionEventID(event Event) string {
	switch e := event.(type) {
	case *ToolCallConfirmationEvent:
		return e.RequestID
	case *MaxIterationsReachedEvent:
		return e.RequestID
	case *ElicitationRequestEvent:
		return e.RequestID
	default:
		return ""
	}
}

func (h *sessionEventHub) HasSubscribers(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[sessionID]) != 0 || len(h.seqSubs[sessionID]) != 0
}

func (h *sessionEventHub) FenceDelete(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deleting[sessionID] = true
}

// TerminalAndDelete atomically publishes a deletion terminal when the current
// lifecycle has none, then closes all subscribers and clears retained state.
func (h *sessionEventHub) TerminalAndDelete(sessionID, agentName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed[sessionID] {
		return
	}
	h.deleting[sessionID] = true
	if !h.terminal[sessionID] {
		h.publishLocked(sessionID, StreamStopped(sessionID, agentName, "deleted"))
	}
	h.deleteLocked(sessionID)
}

func (h *sessionEventHub) Delete(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deleteLocked(sessionID)
}

func (h *sessionEventHub) deleteLocked(sessionID string) {
	h.closed[sessionID] = true
	for sub := range h.subs[sessionID] {
		h.removeSubscriberLocked(sessionID, sub)
	}
	for sub := range h.seqSubs[sessionID] {
		h.removeSequencedSubscriberLocked(sessionID, sub)
	}
	delete(h.inflight, sessionID)
	delete(h.liveRuns, sessionID)
	delete(h.liveAgent, sessionID)
	delete(h.nextSeq, sessionID)
	delete(h.requestID, sessionID)
	delete(h.generation, sessionID)
	delete(h.terminal, sessionID)
	delete(h.deleting, sessionID)
	delete(h.replay, sessionID)
	delete(h.bytes, sessionID)
}

func (h *sessionEventHub) Close() {
	h.mu.Lock()
	ids := make(map[string]struct{}, len(h.subs)+len(h.seqSubs))
	for id := range h.subs {
		ids[id] = struct{}{}
	}
	for id := range h.seqSubs {
		ids[id] = struct{}{}
	}
	h.mu.Unlock()
	for id := range ids {
		h.Delete(id)
	}
}

func (h *sessionEventHub) removeSubscriberLocked(sessionID string, sub *sessionEventSubscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	delete(h.subs[sessionID], sub)
	if len(h.subs[sessionID]) == 0 {
		delete(h.subs, sessionID)
	}
	close(sub.out)
}

func (h *sessionEventHub) removeSequencedSubscriberLocked(sessionID string, sub *sequencedSessionEventSubscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	delete(h.seqSubs[sessionID], sub)
	if len(h.seqSubs[sessionID]) == 0 {
		delete(h.seqSubs, sessionID)
	}
	close(sub.out)
}

func (h *sessionEventHub) trackInflightLocked(sessionID string, event Event) {
	switch e := event.(type) {
	case *StreamStartedEvent:
		h.terminal[sessionID] = false
		h.liveRuns[sessionID]++
		h.liveAgent[sessionID] = e.AgentName
	case *AgentChoiceEvent:
		if e.Content != "" {
			st := h.inflightLocked(sessionID)
			st.agentName = e.AgentName
			st.content.WriteString(e.Content)
		}
	case *AgentChoiceReasoningEvent:
		if e.Content != "" {
			st := h.inflightLocked(sessionID)
			st.agentName = e.AgentName
			st.reasoning.WriteString(e.Content)
		}
	case *UserMessageEvent, *MessageAddedEvent, *ErrorEvent:
		delete(h.inflight, sessionID)
	case *StreamStoppedEvent:
		delete(h.inflight, sessionID)
		if h.liveRuns[sessionID] > 1 {
			h.liveRuns[sessionID]--
		} else {
			delete(h.liveRuns, sessionID)
			delete(h.liveAgent, sessionID)
		}
	}
}

func (h *sessionEventHub) inflightLocked(sessionID string) *inflightAssistant {
	st := h.inflight[sessionID]
	if st == nil {
		st = &inflightAssistant{}
		h.inflight[sessionID] = st
	}
	return st
}
