package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// PersistenceObserver is the stock [EventObserver] that mirrors the
// runtime's event stream to a [session.Store]:
//
//   - persists the initial session row on [OnRunStart] for non-sub-session runs;
//   - tracks streaming assistant content (AgentChoice and
//     AgentChoiceReasoning) into a single growing message row, finalised
//     on [MessageAddedEvent];
//   - persists user messages, sub-session attachments, summaries, token
//     usage, and session-title updates as they fly past.
//
// Sub-session and SessionScoped-mismatch filtering live inside [OnEvent]
// so callers don't have to think about them.
//
// The runtime auto-registers one of these in [NewLocalRuntime] against
// the configured store. Custom sinks (telemetry, audit, A2A, ...) layer
// alongside via [WithEventObserver].
type PersistenceObserver struct {
	store session.Store

	mu        sync.Mutex
	streaming map[string]*streamingState
}

// streamingState holds the in-flight streaming assistant message for one
// session across consecutive AgentChoice / AgentChoiceReasoning events. It is
// keyed by session id in PersistenceObserver so concurrent runs for different
// sessions on the same runtime cannot corrupt each other's streaming row.
type streamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	agentName        string
	messageID        int64 // ID of the in-flight row, 0 for none.
}

// newPersistenceObserver returns an observer that persists to store, or
// nil when store is nil so the constructor can call [WithEventObserver]
// unconditionally without a guard.
func newPersistenceObserver(store session.Store) *PersistenceObserver {
	if store == nil {
		return nil
	}
	return &PersistenceObserver{store: store, streaming: map[string]*streamingState{}}
}

// OnRunStart persists the session row before the run loop starts.
// Sub-sessions skip this: the parent session's store absorbs them via
// the SubSessionCompletedEvent handling in OnEvent.
func (p *PersistenceObserver) OnRunStart(ctx context.Context, sess *session.Session) {
	if sess.IsSubSession() {
		return
	}
	if err := p.store.UpdateSession(ctx, sess); err != nil {
		slog.WarnContext(ctx, "Failed to persist initial session", "session_id", sess.ID, "error", err)
	}
}

// OnEvent applies the per-event-type persistence rules. Sub-session
// events are skipped (the parent absorbs them on SubSessionCompleted),
// and any [SessionScoped] event tagged with a different session id
// (forwarded sub-agent streaming events) is filtered out so it can't
// pollute the parent's transcript.
func (p *PersistenceObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	if sess.IsSubSession() {
		return
	}
	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != sess.ID {
		return
	}

	switch e := event.(type) {
	case *AgentChoiceEvent:
		p.withStreaming(e.SessionID, func(st *streamingState) {
			st.content.WriteString(e.Content)
			st.agentName = e.AgentName
			p.persistStreamingContentLocked(ctx, e.SessionID, st)
		})

	case *AgentChoiceReasoningEvent:
		p.withStreaming(e.SessionID, func(st *streamingState) {
			st.reasoningContent.WriteString(e.Content)
			st.agentName = e.AgentName
			p.persistStreamingContentLocked(ctx, e.SessionID, st)
		})

	case *UserMessageEvent:
		p.resetStreaming(e.SessionID)
		if _, err := p.store.AddMessage(ctx, e.SessionID, session.UserMessage(e.Message, e.MultiContent...)); err != nil {
			slog.WarnContext(ctx, "Failed to persist user message", "session_id", e.SessionID, "error", err)
		}

	case *MessageAddedEvent:
		// Finalise the streaming row (if any) with the canonical
		// MessageAddedEvent payload, then reset for the next stream.
		st := p.takeStreaming(e.SessionID)
		var err error
		if st != nil && st.messageID != 0 {
			err = p.store.UpdateMessage(ctx, e.SessionID, st.messageID, e.Message)
		} else {
			_, err = p.store.AddMessage(ctx, e.SessionID, e.Message)
		}
		if err != nil {
			var messageID int64
			if st != nil {
				messageID = st.messageID
			}
			slog.WarnContext(ctx, "Failed to persist message",
				"session_id", e.SessionID, "message_id", messageID, "error", err)
		}

	case *SubSessionCompletedEvent:
		if subSess, ok := e.SubSession.(*session.Session); ok {
			if err := p.store.AddSubSession(ctx, e.ParentSessionID, subSess); err != nil {
				slog.WarnContext(ctx, "Failed to persist sub-session", "parent_id", e.ParentSessionID, "error", err)
			}
		}

	case *SessionSummaryEvent:
		if e.persisted {
			break
		}
		item := session.Item{Summary: e.Summary, FirstKeptEntry: e.FirstKeptEntry, Cost: e.Cost, Model: e.Model}
		if e.Usage != nil {
			// Copy so the persisted item doesn't alias the event's pointer.
			usage := *e.Usage
			item.Usage = &usage
		}
		if err := p.store.AddSummary(ctx, e.SessionID, item); err != nil {
			slog.WarnContext(ctx, "Failed to persist summary", "session_id", e.SessionID, "error", err)
		}

	case *TokenUsageEvent:
		if e.Usage != nil {
			if err := p.store.UpdateSessionTokens(ctx, sess.ID, e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost); err != nil {
				slog.WarnContext(ctx, "Failed to persist token usage", "session_id", sess.ID, "error", err)
			}
		}

	case *SessionTitleEvent:
		if err := p.store.UpdateSessionTitle(ctx, sess.ID, e.Title); err != nil {
			slog.WarnContext(ctx, "Failed to persist session title", "session_id", sess.ID, "error", err)
		}

	case *ErrorEvent:
		// Persist agent failures so they survive a session reload and travel
		// with a shared JSON export for diagnostics. Reset the streaming state
		// so any in-flight assistant row is finalised in place and the error is
		// recorded as a distinct trailing item.
		p.resetStreaming(sess.ID)
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		errItem := &session.Error{
			Message:   e.Error,
			Code:      e.Code,
			AgentName: e.AgentName,
			CreatedAt: ts.Format(time.RFC3339),
		}
		if err := p.store.AddError(ctx, sess.ID, errItem); err != nil {
			slog.WarnContext(ctx, "Failed to persist error", "session_id", sess.ID, "error", err)
		}
	}
}

// withStreaming locks the observer, retrieves the streaming state for
// sessionID, and runs fn while holding the lock. The store calls below are
// synchronous today; keeping the lock across create/update preserves event
// ordering for one session while allowing independent sessions to keep separate
// state.
func (p *PersistenceObserver) withStreaming(sessionID string, fn func(*streamingState)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.streaming == nil {
		p.streaming = map[string]*streamingState{}
	}
	st := p.streaming[sessionID]
	if st == nil {
		st = &streamingState{}
		p.streaming[sessionID] = st
	}
	fn(st)
}

func (p *PersistenceObserver) resetStreaming(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.streaming, sessionID)
}

func (p *PersistenceObserver) takeStreaming(sessionID string) *streamingState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.streaming[sessionID]
	delete(p.streaming, sessionID)
	return st
}

// persistStreamingContentLocked creates or updates the streaming assistant
// message row. The runtime emits one AgentChoice / AgentChoiceReasoning
// event per delta chunk, so this fires repeatedly during a streaming
// response; we keep one row open and update it in place rather than
// creating a row per chunk.
func (p *PersistenceObserver) persistStreamingContentLocked(ctx context.Context, sessionID string, st *streamingState) {
	msg := &session.Message{
		AgentName: st.agentName,
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			Content:          st.content.String(),
			ReasoningContent: st.reasoningContent.String(),
		},
	}

	if st.messageID == 0 {
		id, err := p.store.AddMessage(ctx, sessionID, msg)
		if err != nil {
			slog.WarnContext(ctx, "Failed to create streaming message", "session_id", sessionID, "error", err)
			return
		}
		st.messageID = id
		slog.DebugContext(ctx, "[PERSIST] Created streaming message",
			"session_id", sessionID, "message_id", id, "agent", st.agentName)
		return
	}

	if err := p.store.UpdateMessage(ctx, sessionID, st.messageID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to update streaming message",
			"session_id", sessionID, "message_id", st.messageID, "error", err)
	}
}
