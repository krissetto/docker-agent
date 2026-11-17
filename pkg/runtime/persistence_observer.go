package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// PersistenceObserver writes only the emitting session's row. Failed effects
// remain ordered until the driver's completion barrier retries them.
type PersistenceObserver struct {
	store    session.Store
	owner    func(string) *sessionDriver
	mu       sync.Mutex
	journals map[string]*sessionPersistenceJournal
	drain    context.Context //nolint:containedctx // supervisor shutdown context replaces canceled execution lifetime only for final writes
	lifetime context.Context //nolint:containedctx // supervisor-owned cancellation bounds persistence backpressure
}

const (
	persistenceJournalEvents = 256
	persistenceJournalBytes  = 8 << 20
)

type sessionPersistenceJournal struct {
	id         string
	mu         sync.Mutex
	invalidate atomic.Bool
	streaming  *streamingState
	pending    []persistenceEffect
	bytes      int
	// unflushed is set by every queued effect and cleared by each flush attempt,
	// so a batch of events pays for one flush.
	unflushed bool
	failure   error
	terminal  error
}

type persistenceEffect struct {
	write func(context.Context) error
	bytes int
	// chunk is set for streamed deltas of an open row; consecutive head chunks
	// of one stream are written as one append.
	chunk *chunkEffect
}

// chunkEffect appends chunks [firstSeq, firstSeq+len) to the row of st. start is
// set only on the effect that creates the placeholder row.
type chunkEffect struct {
	st        *streamingState
	sessionID string
	start     *session.StreamStart
	firstSeq  int64
	chunks    []session.MessageChunk
	// attempted freezes the batch after a failed write, so a retry repeats the
	// exact same append and never changes its composition.
	attempted bool
}

// bufferedChunk is a delta held in memory until the durability gate opens.
type bufferedChunk struct {
	chunk session.MessageChunk
	bytes int
}

type streamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	presentation     []chat.AssistantPart
	text             chat.AssistantTextAppender
	agentName        string
	messageID        int64
	// logicalID is the assistant message this row belongs to ("" when the producer had none).
	logicalID     string
	writeID       string
	toolCommitted bool
	// Chunked persistence (StreamingMessageStore only).
	buffered    []bufferedChunk
	nextSeq     int64
	definitions map[string]tools.Tool
	// closed is set once the store reports the row was closed by someone else
	// (an edit); later deltas of this stream are dropped, never resurrected.
	closed bool
}

func newPersistenceObserver(store session.Store) *PersistenceObserver {
	if store == nil {
		return nil
	}
	return &PersistenceObserver{store: store, journals: map[string]*sessionPersistenceJournal{}, lifetime: context.Background()} //rubocop:disable Lint/ContextConnectivity
}

func (p *PersistenceObserver) journal(id string) *sessionPersistenceJournal {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.journals == nil {
		p.journals = map[string]*sessionPersistenceJournal{}
	}
	j := p.journals[id]
	if j == nil {
		j = &sessionPersistenceJournal{id: id}
		p.journals[id] = j
	}
	return j
}

// streamingStore reports whether the store can append to and close open rows.
// Other stores keep the full-message path.
func (p *PersistenceObserver) streamingStore() (session.StreamingMessageStore, bool) {
	store, ok := p.store.(session.StreamingMessageStore)
	return store, ok
}

func (p *PersistenceObserver) enqueueLocked(ctx context.Context, j *sessionPersistenceJournal, size int, effect func(context.Context) error) {
	p.queueLocked(j, size, effect)
	_ = p.flushLocked(ctx, j)
}

// queueLocked queues an effect without flushing; the caller flushes once per batch.
func (p *PersistenceObserver) queueLocked(j *sessionPersistenceJournal, size int, effect func(context.Context) error) {
	p.appendLocked(j, persistenceEffect{write: effect, bytes: size})
}

func (p *PersistenceObserver) appendLocked(j *sessionPersistenceJournal, effect persistenceEffect) {
	// A single oversized output stays in the session transcript. Do not retain a
	// second queued copy; backpressure this emitting goroutine until it is written.
	size := effect.bytes
	for (len(j.pending) > 0 && (len(j.pending) >= persistenceJournalEvents || j.bytes+size > persistenceJournalBytes)) || size > persistenceJournalBytes {
		retryCtx, cancel := context.WithTimeout(p.durabilityContext(), defaultSubagentPersistenceTimeout)
		err := p.flushLocked(retryCtx, j)
		if err == nil && size > persistenceJournalBytes {
			err = p.writeEffect(retryCtx, j.id, effect.write)
		}
		cancel()
		if err == nil {
			if size > persistenceJournalBytes {
				return
			}
			break
		}
		j.failure = err
		select {
		case <-p.durabilityContext().Done():
			j.terminal = err
			return
		case <-time.After(subagentPersistenceRetryBase):
		}
	}
	j.pending = append(j.pending, effect)
	j.bytes += size
	j.unflushed = true
}

func (p *PersistenceObserver) flushLocked(ctx context.Context, j *sessionPersistenceJournal) error {
	j.unflushed = false
	if j.terminal != nil {
		return j.terminal
	}
	for len(j.pending) > 0 {
		p.coalesceChunkHeadLocked(j)
		effect := j.pending[0]
		if err := p.writeEffect(ctx, j.id, effect.write); err != nil {
			if effect.chunk != nil {
				effect.chunk.attempted = true
			}
			// Execution cancellation after a failed write must not replace its storage cause.
			if j.failure != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) && !session.IsTemporary(j.failure) {
				return j.failure
			}
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				j.failure = &session.TemporaryError{Err: err}
			} else {
				j.failure = err
			}
			return j.failure
		}
		j.bytes -= effect.bytes
		j.pending[0] = persistenceEffect{}
		j.pending = j.pending[1:]
	}
	j.failure = nil
	return nil
}

// coalesceChunkHeadLocked merges consecutive unattempted chunk effects of one
// stream at the head of the queue into a single append, i.e. one transaction.
func (p *PersistenceObserver) coalesceChunkHeadLocked(j *sessionPersistenceJournal) {
	head := j.pending[0].chunk
	if head == nil || head.attempted {
		return
	}
	n, total, next := 1, j.pending[0].bytes, head.firstSeq+int64(len(head.chunks))
	chunks := len(head.chunks)
	for n < len(j.pending) {
		follower := j.pending[n].chunk
		if follower == nil || follower.attempted || follower.start != nil || follower.st != head.st || follower.firstSeq != next {
			break
		}
		next += int64(len(follower.chunks))
		chunks += len(follower.chunks)
		total += j.pending[n].bytes
		n++
	}
	if n == 1 {
		return
	}
	merged := &chunkEffect{st: head.st, sessionID: head.sessionID, start: head.start, firstSeq: head.firstSeq, chunks: make([]session.MessageChunk, 0, chunks)}
	for i := range n {
		merged.chunks = append(merged.chunks, j.pending[i].chunk.chunks...)
	}
	effect := persistenceEffect{write: p.chunkWriter(merged), bytes: total, chunk: merged}
	j.pending = append([]persistenceEffect{effect}, j.pending[n:]...)
}

func (p *PersistenceObserver) completionError(id string) error {
	return p.completionErrorContext(p.durabilityContext(), id)
}

func (p *PersistenceObserver) completionErrorContext(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
	defer cancel()
	return p.flushContext(ctx, id)
}

func (p *PersistenceObserver) flushContext(ctx context.Context, id string) error {
	j := p.journal(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	return p.flushLocked(ctx, j)
}

// release drops the journal. Callers hold the registry lock, so the open
// streaming row (if any) is compacted off-lock. Dropping the journal already
// forgets the stream, so compaction is invisible: no later run can continue it.
// This covers a canceled StreamStopped that was never delivered to observe.
func (p *PersistenceObserver) release(id string) {
	p.mu.Lock()
	j := p.journals[id]
	delete(p.journals, id)
	p.mu.Unlock()
	if j != nil {
		go p.compactReleasedStream(id, j)
	}
}

// compactReleasedStream closes the open row of a released journal. A row whose
// placeholder was never written has nothing to close; a row that a failed write
// left behind keeps the content that was actually committed.
func (p *PersistenceObserver) compactReleasedStream(id string, j *sessionPersistenceJournal) {
	store, ok := p.streamingStore()
	if !ok {
		return
	}
	j.mu.Lock()
	st := j.streaming
	j.streaming = nil
	open := st != nil && st.writeID != "" && st.messageID != 0 && !st.closed
	var rowID int64
	if open {
		rowID = st.messageID
	}
	j.mu.Unlock()
	if !open {
		return
	}
	ctx, cancel := context.WithTimeout(p.durabilityContext(), defaultSubagentPersistenceTimeout)
	defer cancel()
	err := store.CloseStreamingMessage(ctx, id, rowID, nil)
	if err != nil && !errors.Is(err, session.ErrStreamClosed) && !errors.Is(err, session.ErrNotFound) {
		slog.WarnContext(ctx, "Compacting released stream failed", "session_id", id, "error", err)
	}
}

func (p *PersistenceObserver) appendItem(ctx context.Context, sessionID, writeID string, item session.Item) (int64, error) {
	if store, ok := p.store.(session.ItemAppender); ok {
		return store.AppendItem(ctx, sessionID, writeID, item)
	}
	switch {
	case item.Message != nil:
		return p.store.AddMessage(ctx, sessionID, item.Message)
	case item.Error != nil:
		return 0, p.store.AddError(ctx, sessionID, item.Error)
	case item.SubSession != nil:
		return 0, p.store.AddSubSession(ctx, sessionID, item.SubSession)
	case item.Summary != "":
		return 0, p.store.AddSummary(ctx, sessionID, item)
	default:
		return 0, errors.New("unsupported persistence item")
	}
}

func (p *PersistenceObserver) OnRunStart(ctx context.Context, sess *session.Session) {
	// Open streaming rows are deliberately not compacted here: session ownership
	// is process-local (SessionService claims), so another process may still be
	// streaming into the same row. Readers reconstruct open rows instead.
	j := p.journal(sess.ID)
	j.mu.Lock()
	defer j.mu.Unlock()
	snapshot := sess.OwnSnapshot()
	snapshot.Messages = nil
	data, _ := json.Marshal(snapshot)
	p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
		latest := sess.OwnSnapshot()
		if p.owner != nil {
			if d := p.owner(sess.ID); d != nil {
				// writeEffect already holds the owner's durable I/O lane, so
				// this snapshot cannot precede an unacknowledged accounting save.
				var err error
				latest, err = d.ownerSnapshot(ctx)
				if err != nil {
					return err
				}
			}
		}
		latest.Messages = nil
		return p.store.UpdateSession(ctx, latest)
	})
}

// OnEvent persists one event synchronously; see onEvents.
func (p *PersistenceObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	p.onEvents(ctx, sess, []Event{event})
}

// onEvents applies events in order under one journal lock and flushes once for
// the whole batch. Effects keep their FIFO order, so a failed flush leaves the
// same retry queue that per-event flushing would have left.
func (p *PersistenceObserver) onEvents(ctx context.Context, sess *session.Session, events []Event) {
	id := sess.ID
	inScope := func(event Event) bool {
		scoped, ok := event.(SessionScoped)
		return !ok || scoped.GetSessionID() == id
	}
	if !slices.ContainsFunc(events, inScope) {
		return
	}
	j := p.journal(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, event := range events {
		if inScope(event) {
			p.applyEventLocked(ctx, id, j, event)
		}
	}
	if j.unflushed {
		_ = p.flushLocked(ctx, j)
	}
	if err := j.failure; err != nil {
		slog.WarnContext(ctx, "Session persistence awaiting retry", "session_id", id, "error", err)
	}
}

func (p *PersistenceObserver) applyEventLocked(ctx context.Context, id string, j *sessionPersistenceJournal, event Event) {
	if j.invalidate.Swap(false) {
		p.endStreamLocked(id, j)
	}
	appendItem := func(item session.Item, receipt *session.Message) {
		writeID, err := newSessionRequestID()
		if err != nil {
			j.failure = err
			return
		}
		data, _ := json.Marshal(item)
		p.queueLocked(j, len(data), func(ctx context.Context) error {
			rowID, err := p.appendItem(ctx, id, writeID, item)
			if err == nil && item.Message != nil {
				err = p.publishMessageID(ctx, id, rowID, receipt)
			}
			return err
		})
	}
	switch e := event.(type) {
	case *AgentChoiceEvent:
		st := p.streamForMessageLocked(id, j, e.MessageID)
		st.content.WriteString(e.Content)
		st.presentation = st.text.Append(st.presentation, chat.AssistantPart{Type: chat.AssistantPartContent, Text: e.Content})
		st.agentName = e.AgentName
		p.persistStreamLocked(ctx, id, j, st, session.MessageChunk{Kind: session.MessageChunkContent, Text: e.Content}, len(e.Content))
	case *AgentChoiceReasoningEvent:
		st := p.streamForMessageLocked(id, j, e.MessageID)
		st.reasoningContent.WriteString(e.Content)
		st.presentation = st.text.Append(st.presentation, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: e.Content})
		st.agentName = e.AgentName
		p.persistStreamLocked(ctx, id, j, st, session.MessageChunk{Kind: session.MessageChunkReasoning, Text: e.Content}, len(e.Content))
	case *PartialToolCallEvent:
		st := j.openStreamLocked()
		st.agentName = e.AgentName
		p.persistToolLocked(ctx, id, j, st, e)
	case *ToolCallEvent:
		st := j.streaming
		if st == nil {
			if e.ToolDefinition.Category != "harness" {
				break
			}
			st = j.openStreamLocked()
		}
		st.agentName = e.AgentName
		st.toolCommitted = true
		p.persistToolLocked(ctx, id, j, st, e)
	case *ToolCallResponseEvent:
		if st := j.streaming; st != nil {
			st.toolCommitted = true
			p.persistToolLocked(ctx, id, j, st, e)
		}
	case *UserMessageEvent:
		p.endStreamLocked(id, j)
		msg := QueuedMessage{MessageID: e.MessageID, Content: e.Message, MultiContent: e.MultiContent, InputOrigin: e.InputOrigin, SenderID: e.SenderID, SenderName: e.SenderName, ReportOutcome: e.ReportOutcome, InputMode: e.InputMode}
		message := msg.sessionMessage()
		message.TurnID = e.TurnID
		appendItem(session.NewMessageItem(message), e.ownerMessage)
	case *MessageAddedEvent:
		if e.CommittedRole() == chat.MessageRoleUser {
			p.endStreamLocked(id, j)
		}
		if e.CommittedRole() != chat.MessageRoleAssistant {
			if e.Message != nil && !e.boundaryOnly {
				message := *e.Message
				appendItem(session.NewMessageItem(&message), e.ownerMessage)
			}
			break
		}
		st := j.streaming
		j.streaming = nil
		if e.boundaryOnly && (e.Message.Message.FinishReason != chat.FinishReasonRefusal || st == nil) {
			// A boundary without a refusal ends the partial row as it stands.
			p.compactStreamLocked(id, j, st)
			break
		}
		message := *e.Message
		switch {
		case st != nil && st.writeID != "" && (st.logicalID == "" || st.logicalID == message.Message.MessageID):
			data, _ := json.Marshal(message)
			p.queueLocked(j, len(data), p.settleWriter(id, st, &message, e.ownerMessage))
		case st != nil && st.writeID != "":
			// A different message settled while this row is open. Keep the open row
			// as its own canonical partial, then append the settled message as a row of its own.
			p.compactStreamLocked(id, j, st)
			appendItem(session.NewMessageItem(&message), e.ownerMessage)
		default:
			appendItem(session.NewMessageItem(&message), e.ownerMessage)
		}
	case *SubSessionCompletedEvent:
		if child, ok := e.SubSession.(*session.Session); ok && !child.AsyncSubagent {
			ref := child.OwnSnapshot()
			ref.Messages = nil
			data, _ := json.Marshal(ref)
			p.queueLocked(j, len(data), func(ctx context.Context) error { return p.store.AddSubSession(ctx, e.ParentSessionID, ref) })
		}
	case *SessionSummaryEvent:
		if e.persisted {
			return
		}
		item := session.Item{Summary: e.Summary, FirstKeptEntry: e.FirstKeptEntry, Cost: e.Cost, Model: e.Model}
		if e.Usage != nil {
			usage := *e.Usage
			item.Usage = &usage
		}
		appendItem(item, nil)
	case *TokenUsageEvent:
		if e.Usage != nil {
			input, output, cost := e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost
			p.queueLocked(j, 256, func(ctx context.Context) error { return p.store.UpdateSessionTokens(ctx, id, input, output, cost) })
		}
	case *SessionTitleEvent:
		p.queueLocked(j, 256, func(ctx context.Context) error {
			return p.store.UpdateSessionTitle(ctx, id, e.Title)
		})
	case *ErrorEvent:
		p.endStreamLocked(id, j)
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		appendItem(session.Item{Error: &session.Error{Message: e.Error, Code: e.Code, AgentName: e.AgentName, CreatedAt: ts.Format(time.RFC3339)}}, nil)
	case *StreamStoppedEvent:
		// The stop is the run's terminal boundary for every reason: a successful stop
		// with no MessageAdded (for example a whitespace-only turn), a cancel, or an
		// error. A later Retry() has no user event, so without this its deltas would
		// continue the finished row. A cancel can surface as reason error (a model
		// stream canceled by its context returns fatal with no ErrorEvent), so error
		// stops are included. Error stops that do carry an ErrorEvent or settle have
		// already closed the row, so this is a no-op for them. Fallback stores keep
		// today's behavior (their state is untouched).
		if _, chunked := p.streamingStore(); chunked {
			p.endStreamLocked(id, j)
		}
	}
}

// openStreamLocked returns the current stream, starting one if none is open.
func (j *sessionPersistenceJournal) openStreamLocked() *streamingState {
	if j.streaming == nil {
		j.streaming = &streamingState{}
	}
	return j.streaming
}

// streamForMessageLocked returns the stream for a chunk of messageID. A chunk of a
// different assistant message ends the open stream first, so each message keeps
// its own row and no row is ever reused by a later message. Producers that give
// no ID keep joining the open stream, as they always have.
func (p *PersistenceObserver) streamForMessageLocked(id string, j *sessionPersistenceJournal, messageID string) *streamingState {
	if st := j.streaming; st != nil && messageID != "" && st.logicalID != messageID && (st.logicalID != "" || st.writeID != "") {
		p.endStreamLocked(id, j)
	}
	st := j.openStreamLocked()
	if st.logicalID == "" {
		st.logicalID = messageID
	}
	return st
}

// endStreamLocked forgets the current stream and compacts its open row, if any.
func (p *PersistenceObserver) endStreamLocked(id string, j *sessionPersistenceJournal) {
	st := j.streaming
	j.streaming = nil
	p.compactStreamLocked(id, j, st)
}

// compactStreamLocked queues a FIFO compaction of st's open row into one
// canonical message. Missing rows are fine: there is nothing left to compact.
func (p *PersistenceObserver) compactStreamLocked(id string, j *sessionPersistenceJournal, st *streamingState) {
	if st == nil || st.writeID == "" || st.closed {
		return
	}
	store, ok := p.streamingStore()
	if !ok {
		return
	}
	p.appendLocked(j, persistenceEffect{bytes: 256, write: func(ctx context.Context) error {
		err := store.CloseStreamingMessage(ctx, id, st.messageID, nil)
		if errors.Is(err, session.ErrStreamClosed) || errors.Is(err, session.ErrNotFound) {
			return nil
		}
		return err
	}})
}

// persistStreamLocked queues one delta. Deltas before the durability gate opens
// stay in memory; the first durable write creates the placeholder row with them.
func (p *PersistenceObserver) persistStreamLocked(ctx context.Context, id string, j *sessionPersistenceJournal, st *streamingState, chunk session.MessageChunk, bytes int) {
	if _, ok := p.streamingStore(); !ok {
		p.persistStreamingContentLocked(ctx, id, j, st)
		return
	}
	if st.closed || (chunk.Op == nil && chunk.Text == "") {
		return
	}
	if st.writeID == "" {
		st.buffered = append(st.buffered, bufferedChunk{chunk: chunk, bytes: bytes})
		// Uncommitted tool deltas can disappear at a STEERING boundary. Keep their
		// presentation in memory until there is durable assistant output to retain.
		if st.content.Len() == 0 && st.reasoningContent.Len() == 0 && !st.toolCommitted {
			return
		}
		writeID, err := newSessionRequestID()
		if err != nil {
			return
		}
		st.writeID = writeID
		chunks := make([]session.MessageChunk, 0, len(st.buffered))
		total := 0
		for _, buffered := range st.buffered {
			chunks = append(chunks, buffered.chunk)
			total += buffered.bytes
		}
		st.buffered = nil
		st.nextSeq = int64(len(chunks))
		start := &session.StreamStart{WriteID: writeID, AgentName: st.agentName, MessageID: st.logicalID}
		p.appendChunkLocked(j, &chunkEffect{st: st, sessionID: id, start: start, chunks: chunks}, total)
		return
	}
	seq := st.nextSeq
	st.nextSeq++
	p.appendChunkLocked(j, &chunkEffect{st: st, sessionID: id, firstSeq: seq, chunks: []session.MessageChunk{chunk}}, bytes)
}

func (p *PersistenceObserver) appendChunkLocked(j *sessionPersistenceJournal, eff *chunkEffect, bytes int) {
	p.appendLocked(j, persistenceEffect{write: p.chunkWriter(eff), bytes: bytes, chunk: eff})
}

// chunkWriter appends eff's chunks to its row. A closed row means an edit won:
// the stream's remaining deltas are dropped and the effect succeeds.
func (p *PersistenceObserver) chunkWriter(eff *chunkEffect) func(context.Context) error {
	return func(ctx context.Context) error {
		if eff.st.closed {
			return nil
		}
		store, ok := p.streamingStore()
		if !ok {
			return errors.New("session store cannot append streamed chunks")
		}
		if eff.start != nil {
			rowID, err := store.AppendMessageChunks(ctx, eff.sessionID, eff.start, 0, 0, eff.chunks)
			if err != nil {
				return err
			}
			eff.st.messageID = rowID
			return nil
		}
		_, err := store.AppendMessageChunks(ctx, eff.sessionID, nil, eff.st.messageID, eff.firstSeq, eff.chunks)
		if errors.Is(err, session.ErrStreamClosed) {
			eff.st.closed = true
			return nil
		}
		return err
	}
}

// settleWriter writes the final assistant message and then publishes its row ID.
// On a closed row the edit that closed it wins; the message ID is still published.
func (p *PersistenceObserver) settleWriter(id string, st *streamingState, message, receipt *session.Message) func(context.Context) error {
	return func(ctx context.Context) error {
		if store, ok := p.streamingStore(); ok {
			err := store.CloseStreamingMessage(ctx, id, st.messageID, message)
			if errors.Is(err, session.ErrStreamClosed) {
				err = nil
			}
			if err != nil {
				return err
			}
		} else if err := p.store.UpdateMessage(ctx, id, st.messageID, message); err != nil {
			return err
		}
		return p.publishMessageID(ctx, id, st.messageID, receipt)
	}
}

// persistToolLocked applies a tool event to the in-memory presentation and
// persists its durable form when the store supports chunks.
func (p *PersistenceObserver) persistToolLocked(ctx context.Context, id string, j *sessionPersistenceJournal, st *streamingState, event Event) {
	op, ok := toolPresentationOp(event)
	if !ok {
		return
	}
	st.presentation = chat.ApplyAssistantPartOp(st.presentation, op)
	if _, chunked := p.streamingStore(); !chunked {
		p.persistStreamingContentLocked(ctx, id, j, st)
		return
	}
	persisted := st.persistedToolOp(op)
	p.persistStreamLocked(ctx, id, j, st, session.MessageChunk{Kind: session.MessageChunkTool, Op: &persisted}, streamEventBytes(event))
}

// persistedToolOp omits a definition identical to the last one written for the
// same tool call, keeping each op O(delta) instead of O(definition) per delta.
func (st *streamingState) persistedToolOp(op chat.AssistantPartOp) chat.AssistantPartOp {
	if op.Definition == nil || op.ToolCallID == "" {
		return op
	}
	if st.definitions == nil {
		st.definitions = map[string]tools.Tool{}
	}
	if previous, ok := st.definitions[op.ToolCallID]; ok && sameToolDefinition(previous, *op.Definition) {
		op.Definition = nil
		return op
	}
	st.definitions[op.ToolCallID] = *op.Definition
	return op
}

// sameToolDefinition compares every field the store persists. Stricter than
// needed is safe: it only sends the definition again.
func sameToolDefinition(a, b tools.Tool) bool {
	return a.Name == b.Name && a.Category == b.Category && a.Description == b.Description &&
		reflect.DeepEqual(a.Annotations, b.Annotations) &&
		reflect.DeepEqual(a.Parameters, b.Parameters) &&
		reflect.DeepEqual(a.OutputSchema, b.OutputSchema)
}

// streamEventBytes is the payload a streamed event adds to its row: the text
// bytes, or the marshaled tool op with its full definition (an upper bound).
func streamEventBytes(event Event) int {
	switch e := event.(type) {
	case *AgentChoiceEvent:
		return len(e.Content)
	case *AgentChoiceReasoningEvent:
		return len(e.Content)
	}
	op, ok := assistantToolOp(event)
	if !ok {
		return 0
	}
	data, _ := json.Marshal(op)
	return len(data)
}

func (p *PersistenceObserver) persistStreamingContentLocked(ctx context.Context, id string, j *sessionPersistenceJournal, st *streamingState) {
	// Uncommitted tool deltas can disappear at a STEERING boundary. Keep their
	// presentation in memory until there is durable assistant output to retain.
	if st.content.Len() == 0 && st.reasoningContent.Len() == 0 && !st.toolCommitted {
		return
	}
	message := &session.Message{AgentName: st.agentName, Message: chat.Message{
		MessageID: st.logicalID, Role: chat.MessageRoleAssistant, Content: st.content.String(), ReasoningContent: st.reasoningContent.String(),
		Presentation: cloneAssistantPresentation(st.presentation),
	}}
	data, _ := json.Marshal(message)
	if st.writeID == "" {
		writeID, err := newSessionRequestID()
		if err != nil {
			return
		}
		st.writeID = writeID
		p.queueLocked(j, len(data), func(ctx context.Context) error {
			rowID, err := p.appendItem(ctx, id, writeID, session.NewMessageItem(message))
			if err == nil {
				st.messageID = rowID
			}
			return err
		})
	} else {
		p.queueLocked(j, len(data), func(ctx context.Context) error {
			return p.store.UpdateMessage(ctx, id, st.messageID, message)
		})
	}
}

func (p *PersistenceObserver) pendingError(id string) error {
	j := p.journal(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failure
}

func (p *PersistenceObserver) setDrainContext(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.drain = ctx
}

func (p *PersistenceObserver) durabilityContext() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.drain != nil {
		return p.drain
	}
	return p.lifetime
}

func (p *PersistenceObserver) writeEffect(ctx context.Context, id string, write func(context.Context) error) error {
	if p.owner != nil {
		if d := p.owner(id); d != nil {
			return d.durableIOContext(ctx, func() (sessionIOReservation, error) {
				return sessionIOReservation{write: write}, nil
			}, true)
		}
	}
	return write(ctx)
}

func (p *PersistenceObserver) invalidateTranscript(id string) {
	p.journal(id).invalidate.Store(true)
}

func (p *PersistenceObserver) publishMessageID(ctx context.Context, id string, rowID int64, receipt *session.Message) error {
	if p.owner == nil || receipt == nil {
		return nil
	}
	d := p.owner(id)
	if d == nil {
		return nil
	}
	return d.ownerCall(context.WithoutCancel(ctx), func() error {
		d.sess.PublishMessageID(receipt, rowID)
		return nil
	})
}

// capturePresentation routes streamed text through the append-only text appender.
func capturePresentation(text *chat.AssistantTextAppender, parts []chat.AssistantPart, event Event) []chat.AssistantPart {
	switch e := event.(type) {
	case *AgentChoiceEvent:
		return text.Append(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: e.Content})
	case *AgentChoiceReasoningEvent:
		return text.Append(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: e.Content})
	}
	return captureAssistantPresentation(parts, event)
}

func captureAssistantPresentation(parts []chat.AssistantPart, event Event) []chat.AssistantPart {
	switch e := event.(type) {
	case *AgentChoiceEvent:
		return chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: e.Content})
	case *AgentChoiceReasoningEvent:
		return chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: e.Content})
	}
	op, ok := toolPresentationOp(event)
	if !ok {
		return parts
	}
	return chat.ApplyAssistantPartOp(parts, op)
}

// toolPresentationOp returns the presentation op of a tool event with its
// definition cloned, so the op can outlive the event.
func toolPresentationOp(event Event) (chat.AssistantPartOp, bool) {
	op, ok := assistantToolOp(event)
	if ok && op.Definition != nil {
		definition := cloneLiveToolDefinition(*op.Definition)
		op.Definition = &definition
	}
	return op, ok
}

// assistantToolOp maps a tool event to its presentation op. The op's definition
// still aliases the event; use toolPresentationOp when the op outlives the event.
func assistantToolOp(event Event) (chat.AssistantPartOp, bool) {
	switch e := event.(type) {
	case *PartialToolCallEvent:
		return chat.AssistantPartOp{
			Kind:       chat.AssistantPartOpToolDelta,
			ToolCallID: e.ToolCall.ID,
			Type:       string(e.ToolCall.Type),
			Name:       e.ToolCall.Function.Name,
			Arguments:  e.ToolCall.Function.Arguments,
			Definition: e.ToolDefinition,
		}, true
	case *ToolCallEvent:
		call := e.ToolCall
		definition := e.ToolDefinition
		return chat.AssistantPartOp{Kind: chat.AssistantPartOpToolCall, ToolCallID: call.ID, Call: &call, Definition: &definition}, true
	case *ToolCallResponseEvent:
		result := e.Response
		op := chat.AssistantPartOp{Kind: chat.AssistantPartOpToolResult, ToolCallID: e.ToolCallID, Result: &result}
		if e.Result != nil {
			op.IsError = e.Result.IsError
		}
		return op, true
	}
	return chat.AssistantPartOp{}, false
}

func cloneAssistantPresentation(parts []chat.AssistantPart) []chat.AssistantPart {
	cloned := slices.Clone(parts)
	for i := range cloned {
		if tool := cloned[i].Tool; tool != nil {
			copyTool := *tool
			copyTool.Definition = cloneLiveToolDefinition(tool.Definition)
			if tool.Result != nil {
				result := *tool.Result
				copyTool.Result = &result
			}
			cloned[i].Tool = &copyTool
		}
	}
	return cloned
}
