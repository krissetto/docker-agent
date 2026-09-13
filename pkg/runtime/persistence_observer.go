package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// PersistenceObserver writes only the emitting session's row. Failed effects
// remain ordered until the driver's completion barrier retries them.
type PersistenceObserver struct {
	store    session.Store
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
	mu        sync.Mutex
	streaming *streamingState
	pending   []persistenceEffect
	bytes     int
	failure   error
	terminal  error
}

type persistenceEffect struct {
	write func(context.Context) error
	bytes int
}

type streamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	agentName        string
	messageID        int64
	writeID          string
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
		j = &sessionPersistenceJournal{}
		p.journals[id] = j
	}
	return j
}

func (p *PersistenceObserver) enqueueLocked(ctx context.Context, j *sessionPersistenceJournal, size int, effect func(context.Context) error) {
	// A single oversized output stays in the session transcript. Do not retain a
	// second queued copy; backpressure this emitting goroutine until it is written.
	for (len(j.pending) > 0 && (len(j.pending) >= persistenceJournalEvents || j.bytes+size > persistenceJournalBytes)) || size > persistenceJournalBytes {
		retryCtx, cancel := context.WithTimeout(p.durabilityContext(), defaultSubagentPersistenceTimeout)
		err := p.flushLocked(retryCtx, j)
		if err == nil && size > persistenceJournalBytes {
			err = effect(retryCtx)
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
	j.pending = append(j.pending, persistenceEffect{write: effect, bytes: size})
	j.bytes += size
	_ = p.flushLocked(ctx, j)
}

func (p *PersistenceObserver) flushLocked(ctx context.Context, j *sessionPersistenceJournal) error {
	if j.terminal != nil {
		return j.terminal
	}
	for len(j.pending) > 0 {
		effect := j.pending[0]
		if err := effect.write(ctx); err != nil {
			// Execution cancellation after a failed write must not replace its storage cause.
			if j.failure != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return j.failure
			}
			j.failure = err
			return err
		}
		j.bytes -= effect.bytes
		j.pending[0] = persistenceEffect{}
		j.pending = j.pending[1:]
	}
	j.failure = nil
	return nil
}

func (p *PersistenceObserver) completionError(id string) error {
	j := p.journal(id)
	j.mu.Lock()
	defer j.mu.Unlock()
	ctx, cancel := context.WithTimeout(p.durabilityContext(), defaultSubagentPersistenceTimeout)
	defer cancel()
	return p.flushLocked(ctx, j)
}

func (p *PersistenceObserver) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.journals, id)
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
	j := p.journal(sess.ID)
	j.mu.Lock()
	defer j.mu.Unlock()
	snapshot := sess.OwnSnapshot()
	snapshot.Messages = nil
	data, _ := json.Marshal(snapshot)
	p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error { return p.store.UpdateSession(ctx, snapshot) })
}

func (p *PersistenceObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != sess.ID {
		return
	}
	j := p.journal(sess.ID)
	j.mu.Lock()
	defer j.mu.Unlock()
	id := sess.ID
	appendItem := func(item session.Item) {
		writeID, err := newSessionRequestID()
		if err != nil {
			j.failure = err
			return
		}
		data, _ := json.Marshal(item)
		p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error { _, err := p.appendItem(ctx, id, writeID, item); return err })
	}
	switch e := event.(type) {
	case *AgentChoiceEvent:
		st := j.streaming
		if st == nil {
			st = &streamingState{}
			j.streaming = st
		}
		st.content.WriteString(e.Content)
		st.agentName = e.AgentName
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *AgentChoiceReasoningEvent:
		st := j.streaming
		if st == nil {
			st = &streamingState{}
			j.streaming = st
		}
		st.reasoningContent.WriteString(e.Content)
		st.agentName = e.AgentName
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *UserMessageEvent:
		j.streaming = nil
		msg := QueuedMessage{Content: e.Message, MultiContent: e.MultiContent, InputOrigin: e.InputOrigin, SenderID: e.SenderID, SenderName: e.SenderName, InputMode: e.InputMode}
		message := msg.sessionMessage()
		message.TurnID = e.TurnID
		appendItem(session.NewMessageItem(message))
	case *MessageAddedEvent:
		st := j.streaming
		j.streaming = nil
		if e.boundaryOnly {
			break
		}
		message := *e.Message
		if st != nil && st.writeID != "" {
			p.enqueueLocked(ctx, j, len(message.Message.Content)+len(message.Message.ReasoningContent)+128, func(ctx context.Context) error { return p.store.UpdateMessage(ctx, id, st.messageID, &message) })
		} else {
			appendItem(session.NewMessageItem(&message))
		}
	case *SubSessionCompletedEvent:
		if child, ok := e.SubSession.(*session.Session); ok && !child.AsyncSubagent {
			ref := child.OwnSnapshot()
			ref.Messages = nil
			data, _ := json.Marshal(ref)
			p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error { return p.store.AddSubSession(ctx, e.ParentSessionID, ref) })
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
		appendItem(item)
	case *TokenUsageEvent:
		if e.Usage != nil {
			input, output, cost := e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost
			p.enqueueLocked(ctx, j, 256, func(ctx context.Context) error { return p.store.UpdateSessionTokens(ctx, id, input, output, cost) })
		}
	case *SessionTitleEvent:
		p.enqueueLocked(ctx, j, 256, func(ctx context.Context) error { return p.store.UpdateSessionTitle(ctx, id, e.Title) })
	case *ErrorEvent:
		j.streaming = nil
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		appendItem(session.Item{Error: &session.Error{Message: e.Error, Code: e.Code, AgentName: e.AgentName, CreatedAt: ts.Format(time.RFC3339)}})
	}
	if err := j.failure; err != nil {
		slog.WarnContext(ctx, "Session persistence awaiting retry", "session_id", id, "error", err)
	}
}

func (p *PersistenceObserver) persistStreamingContentLocked(ctx context.Context, id string, j *sessionPersistenceJournal, st *streamingState) {
	message := &session.Message{AgentName: st.agentName, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: st.content.String(), ReasoningContent: st.reasoningContent.String()}}
	if st.writeID == "" {
		writeID, err := newSessionRequestID()
		if err != nil {
			return
		}
		st.writeID = writeID
		p.enqueueLocked(ctx, j, len(message.Message.Content)+len(message.Message.ReasoningContent)+128, func(ctx context.Context) error {
			rowID, err := p.appendItem(ctx, id, writeID, session.NewMessageItem(message))
			if err == nil {
				st.messageID = rowID
			}
			return err
		})
	} else {
		p.enqueueLocked(ctx, j, len(message.Message.Content)+len(message.Message.ReasoningContent)+128, func(ctx context.Context) error {
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
