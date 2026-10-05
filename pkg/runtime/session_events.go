package runtime

import (
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	defaultSessionEventReplayCapacity = 1024
	// Subscriber limits are independent of caller buffers and replay retention.
	maxSessionEventSubscriberBuffer = 1024
	maxSessionEventSubscribers      = 256
	maxSessionEventSubscriberBytes  = 8 << 20
)

// SequencedSessionEvent is a replayable session event. Gap marks that events
// after the requested cursor were evicted and the consumer must resnapshot.
type SequencedSessionEvent struct {
	Epoch          string
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
	subscriberCount int
	epoch           string
	mu              sync.Mutex
	subs            map[string]map[*sessionEventSubscriber]struct{}
	seqSubs         map[string]map[*sequencedSessionEventSubscriber]struct{}
	publicSubs      map[string]map[*publicSessionEventSubscriber]struct{}
	inflight        map[string]*inflightAssistant
	activeTools     map[string][]*inflightTool
	outputBytes     map[string]int
	liveRuns        map[string]int
	liveAgent       map[string]string
	nextSeq         map[string]uint64
	requestID       map[string]string
	generation      map[string]uint64
	terminal        map[string]bool
	deleting        map[string]bool
	closed          map[string]bool
	replay          map[string][]retainedSessionEvent
	capacity        int
	maxBytes        int
	bytes           map[string]int
}

type sessionEventSubscriber struct {
	queuedBytes int
	queuedSizes []int
	out         chan Event
	limit       int
	closed      bool
}

type sequencedSessionEventSubscriber struct {
	queuedBytes int
	queuedSizes []int
	out         chan SequencedSessionEvent
	limit       int
	closed      bool
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
		epoch:       uuid.NewString(),
		subs:        map[string]map[*sessionEventSubscriber]struct{}{},
		seqSubs:     map[string]map[*sequencedSessionEventSubscriber]struct{}{},
		publicSubs:  map[string]map[*publicSessionEventSubscriber]struct{}{},
		inflight:    map[string]*inflightAssistant{},
		activeTools: map[string][]*inflightTool{},
		outputBytes: map[string]int{},
		liveRuns:    map[string]int{},
		liveAgent:   map[string]string{},
		nextSeq:     map[string]uint64{},
		requestID:   map[string]string{},
		generation:  map[string]uint64{},
		terminal:    map[string]bool{},
		deleting:    map[string]bool{},
		closed:      map[string]bool{},
		replay:      map[string][]retainedSessionEvent{},
		capacity:    capacity,
		maxBytes:    maxBytes,
		bytes:       map[string]int{},
	}
}

// Subscribe is the compatibility event surface. Its channel is bounded; a
// slow consumer is disconnected rather than retaining an unbounded queue.
func (h *sessionEventHub) Subscribe(sessionID string, buffer int) (seed []Event, _ <-chan Event, cancel func()) {
	buffer = min(max(buffer, 1), maxSessionEventSubscriberBuffer)
	h.mu.Lock()
	if h.closed[sessionID] || h.subscriberCount >= maxSessionEventSubscribers || !h.liveSeedFitsLocked(sessionID) {
		out := make(chan Event)
		close(out)
		h.mu.Unlock()
		return nil, out, func() {}
	}
	sub := &sessionEventSubscriber{out: make(chan Event, buffer+1), limit: buffer}
	seed = h.liveSeedLocked(sessionID)
	if h.subs[sessionID] == nil {
		h.subs[sessionID] = map[*sessionEventSubscriber]struct{}{}
	}
	h.subs[sessionID][sub] = struct{}{}
	h.subscriberCount++
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
	buffer = min(max(buffer, 1), maxSessionEventSubscriberBuffer)
	h.mu.Lock()
	cursor = h.nextSeq[sessionID]
	if h.closed[sessionID] || h.subscriberCount >= maxSessionEventSubscribers || (since == nil && !h.liveSeedFitsLocked(sessionID)) || (since != nil && !h.replayFitsLocked(sessionID, *since)) {
		out := make(chan SequencedSessionEvent, 1)
		out <- SequencedSessionEvent{Epoch: h.epoch, Gap: true, FirstAvailable: cursor + 1}
		close(out)
		h.mu.Unlock()
		return nil, out, func() {}, cursor
	}
	sub := &sequencedSessionEventSubscriber{out: make(chan SequencedSessionEvent, buffer+1), limit: buffer}
	if since == nil {
		for _, event := range h.liveSeedLocked(sessionID) {
			seed = append(seed, SequencedSessionEvent{Epoch: h.epoch, Event: event})
		}
	} else {
		seed = h.replayLocked(sessionID, *since)
	}
	if h.seqSubs[sessionID] == nil {
		h.seqSubs[sessionID] = map[*sequencedSessionEventSubscriber]struct{}{}
	}
	h.seqSubs[sessionID][sub] = struct{}{}
	h.subscriberCount++
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
	detached, err := observerEventSnapshot(event)
	if err != nil {
		slog.Warn("Cannot detach session event", "error", err)
		return
	}
	event = detached
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
	eventBytes := estimateEventBytes(event) + len(h.requestID[sessionID]) + len(interactionID)
	h.appendReplayLocked(sessionID, retainedSessionEvent{sequence: sequence, requestID: h.requestID[sessionID], interactionID: interactionID, generation: h.generation[sessionID], event: event, bytes: eventBytes})
	for sub := range h.subs[sessionID] {
		reconcileQueuedBytes(&sub.queuedBytes, &sub.queuedSizes, len(sub.out))
		if eventBytes > maxSessionEventSubscriberBytes-sub.queuedBytes {
			h.removeSubscriberLocked(sessionID, sub)
			continue
		}
		if !terminalEvent && len(sub.out) >= sub.limit {
			h.removeSubscriberLocked(sessionID, sub)
			continue
		}
		select {
		case sub.out <- cloneSessionEvent(event):
			sub.queuedBytes += eventBytes
			sub.queuedSizes = append(sub.queuedSizes, eventBytes)
		default:
			h.removeSubscriberLocked(sessionID, sub)
		}
	}
	for sub := range h.seqSubs[sessionID] {
		reconcileQueuedBytes(&sub.queuedBytes, &sub.queuedSizes, len(sub.out))
		if eventBytes > maxSessionEventSubscriberBytes-sub.queuedBytes {
			h.gapSequencedSubscriberLocked(sessionID, sub)
			continue
		}
		if !terminalEvent && len(sub.out) >= sub.limit {
			h.gapSequencedSubscriberLocked(sessionID, sub)
			continue
		}
		select {
		case sub.out <- SequencedSessionEvent{Epoch: h.epoch, Sequence: sequence, RequestID: h.requestID[sessionID], InteractionID: interactionID, Event: cloneSessionEvent(event)}:
			sub.queuedBytes += eventBytes
			sub.queuedSizes = append(sub.queuedSizes, eventBytes)
		default:
			h.gapSequencedSubscriberLocked(sessionID, sub)
		}
	}
	for sub := range h.publicSubs[sessionID] {
		reconcileQueuedBytes(&sub.queuedBytes, &sub.queuedSizes, len(sub.out))
		if eventBytes > maxSessionEventSubscriberBytes-sub.queuedBytes || (!terminalEvent && len(sub.out) >= sub.limit) {
			h.gapPublicSubscriberLocked(sessionID, sub)
			continue
		}
		item := SequencedSessionEvent{Epoch: h.epoch, Sequence: sequence, RequestID: h.requestID[sessionID], InteractionID: interactionID, Event: cloneSessionEvent(event)}
		select {
		case sub.out <- sessionEnvelope(sessionID, item):
			sub.queuedBytes += eventBytes
			sub.queuedSizes = append(sub.queuedSizes, eventBytes)
		default:
			h.gapPublicSubscriberLocked(sessionID, sub)
		}
	}
}

// Channel drains happen outside the hub lock. FIFO reconciliation can overcount
// a concurrent drain, but never underestimate retained payload bytes.
func reconcileQueuedBytes(total *int, sizes *[]int, queued int) {
	consumed := len(*sizes) - queued
	for _, size := range (*sizes)[:consumed] {
		*total -= size
	}
	*sizes = (*sizes)[consumed:]
}

func (h *sessionEventHub) replayFitsLocked(sessionID string, since uint64) bool {
	total := 0
	for _, event := range h.replay[sessionID] {
		if event.sequence > since {
			total += event.bytes
			if total > maxSessionEventSubscriberBytes {
				return false
			}
		}
	}
	return true
}

func (h *sessionEventHub) liveSeedFitsLocked(sessionID string) bool {
	total := 0
	if st := h.inflight[sessionID]; st != nil {
		total = st.content.Len() + st.reasoning.Len()
	}
	for _, tool := range h.activeTools[sessionID] {
		total += tool.arguments.Len() + tool.output.Len()
		if total > maxSessionEventSubscriberBytes {
			return false
		}
	}
	if total > maxSessionEventSubscriberBytes {
		return false
	}
	total = 0
	for _, event := range h.liveSeedWithCloneLocked(sessionID, false) {
		total += estimateEventBytes(event)
		if total > maxSessionEventSubscriberBytes {
			return false
		}
	}
	return true
}

func (h *sessionEventHub) liveSeedLocked(sessionID string) []Event {
	return h.liveSeedWithCloneLocked(sessionID, true)
}

func (h *sessionEventHub) liveSeedWithCloneLocked(sessionID string, clone bool) []Event {
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
	for _, tool := range h.activeTools[sessionID] {
		call := tool.call
		call.Function.Arguments = tool.arguments.String()
		definition := tool.definition
		if clone {
			definition = cloneLiveToolDefinition(definition)
		}
		if tool.running {
			seed = append(seed, ToolCall(call, definition, tool.agentName))
		} else {
			seed = append(seed, PartialToolCall(call, definition, tool.agentName))
		}
		output := tool.output.String()
		if tool.truncated {
			output += "\n[Earlier tool output truncated at the session replay byte limit]\n"
		}
		if output != "" {
			if clone {
				definition = cloneLiveToolDefinition(tool.definition)
			}
			seed = append(seed, ToolCallOutput(call.ID, definition, output, tool.agentName))
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
		replay[0] = retainedSessionEvent{}
		replay = replay[1:]
	}
	h.replay[sessionID] = replay
}

func (h *sessionEventHub) replayLocked(sessionID string, since uint64) []SequencedSessionEvent {
	replay := h.replay[sessionID]
	if len(replay) == 0 {
		if since != h.nextSeq[sessionID] {
			return []SequencedSessionEvent{{Epoch: h.epoch, Gap: true, FirstAvailable: h.nextSeq[sessionID] + 1}}
		}
		return nil
	}
	out := make([]SequencedSessionEvent, 0, len(replay)+1)
	if since > h.nextSeq[sessionID] || replay[0].sequence > since+1 {
		out = append(out, SequencedSessionEvent{Epoch: h.epoch, Gap: true, FirstAvailable: replay[0].sequence})
	}
	for _, retained := range replay {
		if retained.sequence > since {
			out = append(out, SequencedSessionEvent{Epoch: h.epoch, Sequence: retained.sequence, RequestID: retained.requestID, InteractionID: retained.interactionID, Event: cloneSessionEvent(retained.event)})
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
	case *InteractionResolvedEvent:
		return e.InteractionID
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
	return len(h.subs[sessionID]) != 0 || len(h.seqSubs[sessionID]) != 0 || len(h.publicSubs[sessionID]) != 0
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
	for sub := range h.publicSubs[sessionID] {
		h.removePublicSubscriberLocked(sessionID, sub)
	}
	delete(h.inflight, sessionID)
	delete(h.activeTools, sessionID)
	delete(h.outputBytes, sessionID)
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
	ids := make(map[string]struct{}, len(h.subs)+len(h.seqSubs)+len(h.nextSeq))
	for id := range h.subs {
		ids[id] = struct{}{}
	}
	for id := range h.nextSeq {
		ids[id] = struct{}{}
	}
	for id := range h.seqSubs {
		ids[id] = struct{}{}
	}
	for id := range h.publicSubs {
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
	h.subscriberCount--
	delete(h.subs[sessionID], sub)
	if len(h.subs[sessionID]) == 0 {
		delete(h.subs, sessionID)
	}
	close(sub.out)
}

// Overflow is a recoverable cursor gap, never indistinguishable from a clean
// stream close. Reserve the terminal slot (or discard one queued event if a
// prior terminal filled it) so even a stalled observer receives the gap.
func (h *sessionEventHub) gapSequencedSubscriberLocked(sessionID string, sub *sequencedSessionEventSubscriber) {
	if sub.closed {
		return
	}
	gap := SequencedSessionEvent{Epoch: h.epoch, Gap: true, FirstAvailable: h.nextSeq[sessionID]}
	select {
	case sub.out <- gap:
	default:
		select {
		case <-sub.out:
		default:
		}
		sub.out <- gap
	}
	h.removeSequencedSubscriberLocked(sessionID, sub)
}

func (h *sessionEventHub) removeSequencedSubscriberLocked(sessionID string, sub *sequencedSessionEventSubscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	h.subscriberCount--
	delete(h.seqSubs[sessionID], sub)
	if len(h.seqSubs[sessionID]) == 0 {
		delete(h.seqSubs, sessionID)
	}
	close(sub.out)
}

func (h *sessionEventHub) trackInflightLocked(sessionID string, event Event) {
	h.trackToolsLocked(sessionID, event)
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

// Tool arguments are authoritative state, not evictable journal deltas. Output
// gets a separate per-session ReplayBytes budget shared by all active calls.
type inflightTool struct {
	agentName  string
	call       tools.ToolCall
	definition tools.Tool
	arguments  strings.Builder
	running    bool
	output     strings.Builder
	truncated  bool
}

func (h *sessionEventHub) activeToolLocked(sessionID, id string) *inflightTool {
	for _, tool := range h.activeTools[sessionID] {
		if tool.call.ID == id {
			return tool
		}
	}
	tool := &inflightTool{call: tools.ToolCall{ID: id}}
	h.activeTools[sessionID] = append(h.activeTools[sessionID], tool)
	return tool
}

func (h *sessionEventHub) trackToolsLocked(sessionID string, event Event) {
	switch e := event.(type) {
	case *PartialToolCallEvent:
		tool := h.activeToolLocked(sessionID, e.ToolCall.ID)
		tool.agentName = e.AgentName
		if e.ToolCall.Type != "" {
			tool.call.Type = e.ToolCall.Type
		}
		if e.ToolCall.Function.Name != "" {
			tool.call.Function.Name = e.ToolCall.Function.Name
		}
		tool.arguments.WriteString(e.ToolCall.Function.Arguments)
		if e.ToolDefinition != nil {
			tool.definition = cloneLiveToolDefinition(*e.ToolDefinition)
		}
	case *ToolCallEvent:
		tool := h.activeToolLocked(sessionID, e.ToolCall.ID)
		tool.agentName, tool.call, tool.running = e.AgentName, e.ToolCall, true
		tool.call.Function.Arguments = ""
		tool.arguments.Reset()
		tool.arguments.WriteString(e.ToolCall.Function.Arguments)
		tool.definition = cloneLiveToolDefinition(e.ToolDefinition)
	case *ToolCallOutputEvent:
		for _, tool := range h.activeTools[sessionID] {
			if tool.call.ID != e.ToolCallID {
				continue
			}
			if tool.truncated {
				return
			}
			output := chat.TruncateUTF8Bytes(e.Output, max(0, h.maxBytes-h.outputBytes[sessionID]))
			tool.output.WriteString(output)
			h.outputBytes[sessionID] += len(output)
			tool.truncated = len(output) < len(e.Output)
			return
		}
	case *MessageAddedEvent:
		if e.Message == nil || e.Message.Message.Role != chat.MessageRoleAssistant {
			return
		}
		// Committed calls already supply their complete arguments in the snapshot.
		h.activeTools[sessionID] = slices.DeleteFunc(h.activeTools[sessionID], func(tool *inflightTool) bool {
			if tool.running || tool.agentName != e.Message.AgentName {
				return false
			}
			h.outputBytes[sessionID] -= tool.output.Len()
			return true
		})
		if len(h.activeTools[sessionID]) == 0 {
			delete(h.activeTools, sessionID)
			delete(h.outputBytes, sessionID)
		}
	case *ToolCallResponseEvent:
		h.activeTools[sessionID] = slices.DeleteFunc(h.activeTools[sessionID], func(tool *inflightTool) bool {
			if tool.call.ID != e.ToolCallID {
				return false
			}
			h.outputBytes[sessionID] -= tool.output.Len()
			return true
		})
		if len(h.activeTools[sessionID]) == 0 {
			delete(h.activeTools, sessionID)
			delete(h.outputBytes, sessionID)
		}
	case *StreamStoppedEvent:
		if h.liveRuns[sessionID] <= 1 {
			delete(h.activeTools, sessionID)
			delete(h.outputBytes, sessionID)
		}
	}
}

func cloneLiveToolDefinition(tool tools.Tool) tools.Tool {
	tool.Parameters = cloneLiveSchema(tool.Parameters)
	tool.OutputSchema = cloneLiveSchema(tool.OutputSchema)
	tool.Metadata = maps.Clone(tool.Metadata)
	if tool.Annotations.DestructiveHint != nil {
		hint := *tool.Annotations.DestructiveHint
		tool.Annotations.DestructiveHint = &hint
	}
	if tool.Annotations.OpenWorldHint != nil {
		hint := *tool.Annotations.OpenWorldHint
		tool.Annotations.OpenWorldHint = &hint
	}
	return tool
}

func cloneLiveSchema(value any) any {
	if value == nil {
		return nil
	}
	// Schemas may be typed pointers or JSON maps; observers own either representation.
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var cloned any
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil
	}
	return cloned
}

func cloneSessionEvent(event Event) Event {
	if event == nil {
		return nil
	}
	detached, err := observerEventSnapshot(event)
	if err != nil {
		slog.Warn("Cannot detach session event", "error", err)
		return nil
	}
	return detached
}

type publicSessionEventSubscriber struct {
	queuedBytes int
	queuedSizes []int
	out         chan SessionEvent
	limit       int
	closed      bool
	done        chan struct{}
}

// Public observations use the hub's accounted queue directly, without a second buffer.
func (h *sessionEventHub) SubscribePublic(sessionID string, since *uint64, buffer int) (seed []SequencedSessionEvent, events <-chan SessionEvent, cancel func(), cursor uint64, done <-chan struct{}) {
	buffer = min(max(buffer, 1), maxSessionEventSubscriberBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	cursor = h.nextSeq[sessionID]
	if h.closed[sessionID] || h.subscriberCount >= maxSessionEventSubscribers || (since == nil && !h.liveSeedFitsLocked(sessionID)) || (since != nil && !h.replayFitsLocked(sessionID, *since)) {
		out := make(chan SessionEvent, 1)
		out <- sessionEnvelope(sessionID, SequencedSessionEvent{Epoch: h.epoch, Gap: true, FirstAvailable: cursor + 1})
		close(out)
		closed := make(chan struct{})
		close(closed)
		return nil, out, func() {}, cursor, closed
	}
	sub := &publicSessionEventSubscriber{done: make(chan struct{}), out: make(chan SessionEvent, buffer+1), limit: buffer}
	if since == nil {
		for _, event := range h.liveSeedLocked(sessionID) {
			seed = append(seed, SequencedSessionEvent{Epoch: h.epoch, Event: event})
		}
	} else {
		seed = h.replayLocked(sessionID, *since)
	}
	if h.publicSubs[sessionID] == nil {
		h.publicSubs[sessionID] = make(map[*publicSessionEventSubscriber]struct{})
	}
	h.publicSubs[sessionID][sub] = struct{}{}
	h.subscriberCount++
	var once sync.Once
	cancel = func() {
		once.Do(func() { h.mu.Lock(); defer h.mu.Unlock(); h.removePublicSubscriberLocked(sessionID, sub) })
	}
	return seed, sub.out, cancel, cursor, sub.done
}

func (h *sessionEventHub) gapPublicSubscriberLocked(sessionID string, sub *publicSessionEventSubscriber) {
	if sub.closed {
		return
	}
	gap := sessionEnvelope(sessionID, SequencedSessionEvent{Epoch: h.epoch, Gap: true, FirstAvailable: h.nextSeq[sessionID]})
	select {
	case sub.out <- gap:
	default:
		select {
		case <-sub.out:
		default:
		}
		sub.out <- gap
	}
	h.removePublicSubscriberLocked(sessionID, sub)
}

func (h *sessionEventHub) removePublicSubscriberLocked(sessionID string, sub *publicSessionEventSubscriber) {
	if sub.closed {
		return
	}
	sub.closed = true
	h.subscriberCount--
	delete(h.publicSubs[sessionID], sub)
	if len(h.publicSubs[sessionID]) == 0 {
		delete(h.publicSubs, sessionID)
	}
	close(sub.out)
	close(sub.done)
}
