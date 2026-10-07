package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	failure    error
	terminal   error
}

type persistenceEffect struct {
	write func(context.Context) error
	bytes int
}

type streamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	presentation     []chat.AssistantPart
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
		j = &sessionPersistenceJournal{id: id}
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
			err = p.writeEffect(retryCtx, j.id, effect)
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
		if err := p.writeEffect(ctx, j.id, effect.write); err != nil {
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
	p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
		latest := sess.OwnSnapshot()
		if p.owner != nil {
			if d := p.owner(sess.ID); d != nil {
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

func (p *PersistenceObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != sess.ID {
		return
	}
	j := p.journal(sess.ID)
	j.mu.Lock()
	defer j.mu.Unlock()
	id := sess.ID
	if j.invalidate.Swap(false) {
		j.streaming = nil
	}
	appendItem := func(item session.Item, receipt *session.Message) {
		writeID, err := newSessionRequestID()
		if err != nil {
			j.failure = err
			return
		}
		data, _ := json.Marshal(item)
		p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
			rowID, err := p.appendItem(ctx, id, writeID, item)
			if err == nil && item.Message != nil {
				err = p.publishMessageID(ctx, id, rowID, receipt)
			}
			return err
		})
	}
	switch e := event.(type) {
	case *AgentChoiceEvent:
		st := j.streaming
		if st == nil {
			st = &streamingState{}
			j.streaming = st
		}
		st.content.WriteString(e.Content)
		st.presentation = captureAssistantPresentation(st.presentation, e)
		st.agentName = e.AgentName
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *AgentChoiceReasoningEvent:
		st := j.streaming
		if st == nil {
			st = &streamingState{}
			j.streaming = st
		}
		st.reasoningContent.WriteString(e.Content)
		st.presentation = captureAssistantPresentation(st.presentation, e)
		st.agentName = e.AgentName
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *PartialToolCallEvent:
		st := j.streaming
		if st == nil {
			st = &streamingState{}
			j.streaming = st
		}
		st.agentName = e.AgentName
		st.presentation = captureAssistantPresentation(st.presentation, e)
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *ToolCallEvent:
		st := j.streaming
		if st == nil {
			if e.ToolDefinition.Category != "harness" {
				break
			}
			st = &streamingState{}
			j.streaming = st
		}
		st.agentName = e.AgentName
		st.presentation = captureAssistantPresentation(st.presentation, e)
		p.persistStreamingContentLocked(ctx, id, j, st)
	case *ToolCallResponseEvent:
		if st := j.streaming; st != nil {
			st.presentation = captureAssistantPresentation(st.presentation, e)
			p.persistStreamingContentLocked(ctx, id, j, st)
		}
	case *UserMessageEvent:
		j.streaming = nil
		msg := QueuedMessage{Content: e.Message, MultiContent: e.MultiContent, InputOrigin: e.InputOrigin, SenderID: e.SenderID, SenderName: e.SenderName, ReportOutcome: e.ReportOutcome, InputMode: e.InputMode}
		message := msg.sessionMessage()
		message.TurnID = e.TurnID
		appendItem(session.NewMessageItem(message), e.ownerMessage)
	case *MessageAddedEvent:
		if e.CommittedRole() == chat.MessageRoleUser {
			j.streaming = nil
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
		if e.boundaryOnly && (e.Message.Message.FinishReason != chat.FinishReasonRefusal || st == nil || st.writeID == "") {
			break
		}
		message := *e.Message
		if st != nil && st.writeID != "" {
			data, _ := json.Marshal(message)
			p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
				if err := p.store.UpdateMessage(ctx, id, st.messageID, &message); err != nil {
					return err
				}
				return p.publishMessageID(ctx, id, st.messageID, e.ownerMessage)
			})
		} else {
			appendItem(session.NewMessageItem(&message), e.ownerMessage)
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
		appendItem(item, nil)
	case *TokenUsageEvent:
		if e.Usage != nil {
			input, output, cost := e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost
			p.enqueueLocked(ctx, j, 256, func(ctx context.Context) error { return p.store.UpdateSessionTokens(ctx, id, input, output, cost) })
		}
	case *SessionTitleEvent:
		p.enqueueLocked(ctx, j, 256, func(ctx context.Context) error {
			return p.store.UpdateSessionTitle(ctx, id, e.Title)
		})
	case *ErrorEvent:
		j.streaming = nil
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		appendItem(session.Item{Error: &session.Error{Message: e.Error, Code: e.Code, AgentName: e.AgentName, CreatedAt: ts.Format(time.RFC3339)}}, nil)
	}
	if err := j.failure; err != nil {
		slog.WarnContext(ctx, "Session persistence awaiting retry", "session_id", id, "error", err)
	}
}

func (p *PersistenceObserver) persistStreamingContentLocked(ctx context.Context, id string, j *sessionPersistenceJournal, st *streamingState) {
	message := &session.Message{AgentName: st.agentName, Message: chat.Message{
		Role: chat.MessageRoleAssistant, Content: st.content.String(), ReasoningContent: st.reasoningContent.String(),
		Presentation: cloneAssistantPresentation(st.presentation),
	}}
	data, _ := json.Marshal(message)
	if st.writeID == "" {
		writeID, err := newSessionRequestID()
		if err != nil {
			return
		}
		st.writeID = writeID
		p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
			rowID, err := p.appendItem(ctx, id, writeID, session.NewMessageItem(message))
			if err == nil {
				st.messageID = rowID
			}
			return err
		})
	} else {
		p.enqueueLocked(ctx, j, len(data), func(ctx context.Context) error {
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

func captureAssistantPresentation(parts []chat.AssistantPart, event Event) []chat.AssistantPart {
	var call tools.ToolCall
	var definition *tools.Tool
	var result *string
	var isError, delta bool
	switch e := event.(type) {
	case *AgentChoiceEvent:
		return chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: e.Content})
	case *AgentChoiceReasoningEvent:
		return chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: e.Content})
	case *PartialToolCallEvent:
		call, definition, delta = e.ToolCall, e.ToolDefinition, true
	case *ToolCallEvent:
		call, definition = e.ToolCall, &e.ToolDefinition
	case *ToolCallResponseEvent:
		call.ID, definition, result = e.ToolCallID, &e.ToolDefinition, &e.Response
		if e.Result != nil {
			isError = e.Result.IsError
		}
	default:
		return parts
	}
	idx := slices.IndexFunc(parts, func(part chat.AssistantPart) bool {
		return part.Type == chat.AssistantPartToolCall && part.ToolCallID == call.ID
	})
	if idx < 0 {
		if call.ID == "" || result != nil {
			return parts
		}
		parts = chat.AppendAssistantPart(parts, chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: call.ID, Tool: &chat.AssistantTool{}})
		idx = len(parts) - 1
	}
	tool := parts[idx].Tool
	if tool == nil {
		tool = &chat.AssistantTool{}
		parts[idx].Tool = tool
	}
	if result != nil {
		output := *result
		tool.Result, tool.IsError = &output, isError
		return parts
	}
	if delta {
		tool.Call.ID = call.ID
		if call.Type != "" {
			tool.Call.Type = call.Type
		}
		if call.Function.Name != "" {
			tool.Call.Function.Name = call.Function.Name
		}
		tool.Call.Function.Arguments += call.Function.Arguments
	} else {
		arguments := tool.Call.Function.Arguments
		tool.Call = call
		if call.Function.Arguments == "" {
			tool.Call.Function.Arguments = arguments
		}
	}
	if definition != nil {
		tool.Definition = cloneLiveToolDefinition(*definition)
	}
	return parts
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
