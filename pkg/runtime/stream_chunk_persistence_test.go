package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/sqliteutil"
	"github.com/docker/docker-agent/pkg/tools"
)

// countingStreamStore counts chunk appends and can fail one append after it
// commits, modelling an acknowledgment lost after the transaction landed.
type countingStreamStore struct {
	session.Store
	session.ItemAppender

	inner         session.StreamingMessageStore
	appends       atomic.Int64
	failCommitted atomic.Bool

	mu sync.Mutex
	// boundaries lists the cumulative chunk count after each distinct commit.
	boundaries []int
}

func (s *countingStreamStore) AppendMessageChunks(ctx context.Context, sessionID string, start *session.StreamStart, itemID, firstSeq int64, chunks []session.MessageChunk) (int64, error) {
	s.appends.Add(1)
	id, err := s.inner.AppendMessageChunks(ctx, sessionID, start, itemID, firstSeq, chunks)
	if err == nil {
		s.recordCommit(int(firstSeq) + len(chunks))
	}
	if err == nil && s.failCommitted.CompareAndSwap(true, false) {
		return 0, &session.TemporaryError{Err: errors.New("append acknowledgment lost")}
	}
	return id, err
}

// recordCommit notes a committed boundary. An idempotent retry of the same
// append reports the same boundary again and adds nothing.
func (s *countingStreamStore) recordCommit(boundary int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.boundaries); n == 0 || s.boundaries[n-1] != boundary {
		s.boundaries = append(s.boundaries, boundary)
	}
}

func (s *countingStreamStore) commitBoundaries() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.boundaries)
}

func (s *countingStreamStore) CloseStreamingMessage(ctx context.Context, sessionID string, itemID int64, msg *session.Message) error {
	return s.inner.CloseStreamingMessage(ctx, sessionID, itemID, msg)
}

func (s *countingStreamStore) CloseOpenStreams(ctx context.Context, sessionID string) error {
	return s.inner.CloseOpenStreams(ctx, sessionID)
}

func newCountingStreamStore(t *testing.T) *countingStreamStore {
	t.Helper()
	base := coordinationSQLite(t)
	inner, ok := base.(session.StreamingMessageStore)
	require.True(t, ok, "SQLite store must implement StreamingMessageStore")
	_, ok = base.(session.ItemAppender)
	require.True(t, ok)
	return &countingStreamStore{Store: base, ItemAppender: base.(session.ItemAppender), inner: inner}
}

// streamRows returns the assistant message rows, in order, as the store reports them.
func streamRows(sess *session.Session) []*session.Message {
	var rows []*session.Message
	for _, item := range sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			rows = append(rows, item.Message)
		}
	}
	return rows
}

func newStreamSession(t *testing.T, store session.Store, id string) *session.Session {
	t.Helper()
	sess := session.New(session.WithID(id), session.WithTitle("stream"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return sess
}

// driveObserved runs events through observe, the production delivery path, and
// drains the published stream.
func driveObserved(ctx context.Context, r *LocalRuntime, sess *session.Session, events ...Event) {
	inner := make(chan Event, len(events))
	for _, event := range events {
		inner <- event
	}
	close(inner)
	for range r.observe(ctx, sess, inner) {
	}
}

// checkStoredPrefix verifies the stored row against the published deltas. Each
// delta is the fixed 4-byte text "dNN-", so the stored length gives the committed
// count. The store must hold exactly the commit that covers the last published
// delta: the smallest commit boundary at or above it. Equal to published when every
// delta is its own commit; ahead by at most the one in-flight batch otherwise.
func checkStoredPrefix(ctx context.Context, store *countingStreamStore, sessionID string, published int, want string) string {
	loaded, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return err.Error()
	}
	stored := ""
	if rows := streamRows(loaded); len(rows) > 0 {
		stored = rows[len(rows)-1].Message.Content
	}
	expected := -1
	for _, boundary := range store.commitBoundaries() {
		if boundary >= published {
			expected = boundary
			break
		}
	}
	if expected < 0 {
		return fmt.Sprintf("no commit covers %d published deltas", published)
	}
	if len(stored) != 4*expected || !strings.HasPrefix(want, stored) {
		return fmt.Sprintf("stored %q, want exactly the %d committed deltas for %d published", stored, expected, published)
	}
	return ""
}

func TestStreamChunkMirrorSeesExactPrefix(t *testing.T) {
	const deltas = 70
	var want strings.Builder
	for i := range deltas {
		fmt.Fprintf(&want, "d%02d-", i)
	}

	t.Run("sequential delivery", func(t *testing.T) {
		store := newCountingStreamStore(t)
		sess := newStreamSession(t, store, "stream-mirror-sequential")
		persistence := newPersistenceObserver(store)
		persistence.lifetime = t.Context()
		var published int
		var violations []string
		mirror := &fnObserver{onEvent: func(ctx context.Context, _ *session.Session, e Event) {
			if _, ok := e.(*AgentChoiceEvent); ok {
				published++
			}
			if msg := checkStoredPrefix(ctx, store, sess.ID, published, want.String()); msg != "" {
				violations = append(violations, msg)
			}
		}}
		r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence, mirror}}
		// Each delta waits for its publication before the next is sent, so no
		// batch ever holds two deltas and the store must equal the published text.
		inner := make(chan Event)
		out := r.observe(t.Context(), sess, inner)
		for i := range deltas {
			inner <- AgentChoice("root", sess.ID, fmt.Sprintf("d%02d-", i))
			<-out
		}
		close(inner)
		for range out {
		}
		assert.Empty(t, violations)
		assert.Len(t, store.commitBoundaries(), deltas, "every delta is its own commit")
	})

	t.Run("batched delivery", func(t *testing.T) {
		store := newCountingStreamStore(t)
		sess := newStreamSession(t, store, "stream-mirror-batched")
		persistence := newPersistenceObserver(store)
		persistence.lifetime = t.Context()
		var published int
		var violations []string
		mirror := &fnObserver{onEvent: func(ctx context.Context, _ *session.Session, e Event) {
			if _, ok := e.(*AgentChoiceEvent); ok {
				published++
			}
			if msg := checkStoredPrefix(ctx, store, sess.ID, published, want.String()); msg != "" {
				violations = append(violations, msg)
			}
		}}
		r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence, mirror}}
		inner := make(chan Event, deltas)
		for i := range deltas {
			inner <- AgentChoice("root", sess.ID, fmt.Sprintf("d%02d-", i))
		}
		close(inner)
		for range r.observe(t.Context(), sess, inner) {
		}
		assert.Empty(t, violations)
		assert.Less(t, len(store.commitBoundaries()), deltas, "queued deltas share commits")
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		rows := streamRows(loaded)
		require.Len(t, rows, 1)
		assert.Equal(t, want.String(), rows[0].Message.Content)
	})
}

// Uncommitted tool deltas stay in memory: a steering boundary may drop them, so
// nothing is durable until assistant text or a committed tool call arrives.
func TestStreamChunkGateKeepsUncommittedToolDeltasInMemory(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-gate")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	read := PartialToolCall(tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "read", Arguments: "{"}}, tools.Tool{Name: "read"}, "root")
	persistence.OnEvent(t.Context(), sess, read)
	persistence.OnEvent(t.Context(), sess, read)
	require.NoError(t, persistence.completionError(sess.ID))
	assert.Zero(t, store.appends.Load(), "gated tool deltas are not written")
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, streamRows(loaded))

	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "hi"))
	require.NoError(t, persistence.completionError(sess.ID))
	assert.Equal(t, int64(1), store.appends.Load(), "the first durable write carries every buffered delta")
	loaded, err = store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Message.Presentation, 2)
	assert.Equal(t, "hi", rows[0].Message.Presentation[1].Text)
	require.NotNil(t, rows[0].Message.Presentation[0].Tool)
	assert.Equal(t, "{{", rows[0].Message.Presentation[0].Tool.Call.Function.Arguments)
}

func TestStreamChunkSequentialDeltasCommitPerEvent(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-sequential")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	for _, text := range []string{"a", "b", "c", "d"} {
		persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, text))
		require.NoError(t, persistence.pendingError(sess.ID))
	}
	assert.Equal(t, int64(4), store.appends.Load(), "without backlog every delta is its own commit")
	require.NoError(t, persistence.completionError(sess.ID))
}

func TestStreamChunkRetryRepeatsTheSameAppend(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-retry")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	events := []Event{AgentChoice("root", sess.ID, "a"), AgentChoice("root", sess.ID, "b"), AgentChoice("root", sess.ID, "c")}
	store.failCommitted.Store(true)
	persistence.onEvents(t.Context(), sess, events)
	require.Error(t, persistence.pendingError(sess.ID), "the acknowledgment failure is kept for retry")
	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "d"))
	require.NoError(t, persistence.completionError(sess.ID))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1, "retry reuses the open row, never a duplicate")
	assert.Equal(t, "abcd", rows[0].Message.Content)
	assert.Equal(t, int64(3), store.appends.Load(), "the committed batch, its identical retry, then d")
}

func TestStreamChunkCloseParityCompactsToOneCanonicalRow(t *testing.T) {
	tests := []struct {
		name     string
		closing  func(sessionID string) Event
		invalid  bool
		wantRows []string
	}{
		{name: "error", closing: func(id string) Event { return ErrorForSession(id, "boom") }, wantRows: []string{"partial"}},
		{name: "user", closing: func(id string) Event { return UserMessage("next", id, nil) }, wantRows: []string{"partial"}},
		{name: "settle", closing: func(id string) Event {
			return MessageAdded(id, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "final"}), "root")
		}, wantRows: []string{"final"}},
		{name: "boundary", closing: func(id string) Event {
			event := MessageAdded(id, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "partial", FinishReason: chat.FinishReasonStop}), "root")
			event.(*MessageAddedEvent).boundaryOnly = true
			return event
		}, wantRows: []string{"partial"}},
		{name: "normal stop", closing: func(id string) Event { return StreamStopped(id, "root", turnEndReasonNormal) }, wantRows: []string{"partial"}},
		{name: "canceled stop", closing: func(id string) Event { return StreamStopped(id, "root", turnEndReasonCanceled) }, wantRows: []string{"partial"}},
		{name: "invalidate", invalid: true, wantRows: []string{"partial", "after"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newCountingStreamStore(t)
			sess := newStreamSession(t, store, "close-"+tc.name)
			persistence := newPersistenceObserver(store)
			persistence.lifetime = t.Context()
			persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "partial"))
			if tc.invalid {
				persistence.invalidateTranscript(sess.ID)
				persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "after"))
			} else {
				persistence.OnEvent(t.Context(), sess, tc.closing(sess.ID))
			}
			require.NoError(t, persistence.completionError(sess.ID))
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			rows := streamRows(loaded)
			var contents []string
			for _, row := range rows {
				contents = append(contents, row.Message.Content)
			}
			assert.Equal(t, tc.wantRows, contents)
		})
	}
}

// A crash leaves the row open; without proven single-writer ownership it is
// never compacted at run start, so readers must reconstruct it with the same
// counts a settled row has. A later user message only compacts a row the same
// observer opened.
func TestStreamChunkCrashMidStreamReadersReconstructOneRow(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-crash")
	crashed := newPersistenceObserver(store)
	crashed.lifetime = t.Context()
	crashed.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "half "))
	crashed.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "way"))
	require.NoError(t, crashed.pendingError(sess.ID))

	// A restarted process has an empty journal and no memory of the open row.
	restarted := newPersistenceObserver(store)
	restarted.lifetime = t.Context()
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, streamRows(loaded), 1, "readers reconstruct the open row")
	assert.Equal(t, "half way", streamRows(loaded)[0].Message.Content)

	restarted.OnEvent(t.Context(), sess, UserMessage("continue", sess.ID, nil))
	require.NoError(t, restarted.completionError(sess.ID))
	loaded, err = store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.MessagesSnapshot(), 2, "one partial assistant row plus the user row")
	assert.Equal(t, "half way", streamRows(loaded)[0].Message.Content)
}

func TestStreamChunkEditClosesStreamWithoutResurrection(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-edit")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "old"))
	require.NoError(t, persistence.completionError(sess.ID))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rowID := streamRows(loaded)[0].ID

	edited := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "EDITED"})
	require.NoError(t, store.UpdateMessage(t.Context(), sess.ID, rowID, edited))

	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, " late"))
	require.NoError(t, persistence.pendingError(sess.ID), "ErrStreamClosed is success: the edit wins and the run is not cancelled")
	require.NoError(t, persistence.completionError(sess.ID))
	loaded, err = store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, "EDITED", rows[0].Message.Content)
}

// TestStreamChunkReconstructionMatchesInMemoryPresentation replays random
// interleavings of deltas, tool calls, and results; the stored row must equal
// the a289ea8d3 presentation reducer's output after JSON normalization.
func TestStreamChunkReconstructionMatchesInMemoryPresentation(t *testing.T) {
	for seed := range uint64(12) {
		rng := rand.New(rand.NewPCG(seed, 7))
		store := newCountingStreamStore(t)
		sess := newStreamSession(t, store, fmt.Sprintf("parity-%d", seed))
		persistence := newPersistenceObserver(store)
		persistence.lifetime = t.Context()
		var reference []chat.AssistantPart
		var content, reasoning strings.Builder
		// Text opens the stream first: tool events before any stream are not part of it.
		for i := range 60 {
			var event Event
			callID := fmt.Sprintf("call-%d", rng.IntN(3))
			def := tools.Tool{Name: "tool-" + callID, Category: "builtin", Description: fmt.Sprintf("v%d", rng.IntN(2))}
			switch choice := rng.IntN(6); {
			case i == 0 || choice <= 1:
				delta := fmt.Sprintf("t%d ", rng.IntN(100))
				content.WriteString(delta)
				event = AgentChoice("root", sess.ID, delta)
			case choice == 2:
				delta := fmt.Sprintf("r%d ", rng.IntN(100))
				reasoning.WriteString(delta)
				event = AgentChoiceReasoning("root", sess.ID, delta)
			case choice == 3:
				event = PartialToolCall(tools.ToolCall{ID: callID, Type: "function", Function: tools.FunctionCall{Name: def.Name, Arguments: fmt.Sprintf("{%d", rng.IntN(9))}}, def, "root")
			case choice == 4:
				event = ToolCall(tools.ToolCall{ID: callID, Type: "function", Function: tools.FunctionCall{Name: def.Name, Arguments: "{}"}}, def, "root")
			default:
				result := tools.ResultSuccess("ok")
				if rng.IntN(4) == 0 {
					result = tools.ResultError("bad")
				}
				event = ToolCallResponse(callID, def, result, result.Output, "root")
			}
			persistence.OnEvent(t.Context(), sess, event)
			reference = baselineCaptureAssistantPresentation(reference, event)
		}
		require.NoError(t, persistence.completionError(sess.ID))
		persistence.OnEvent(t.Context(), sess, UserMessage("next", sess.ID, nil))
		require.NoError(t, persistence.completionError(sess.ID))
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		rows := streamRows(loaded)
		require.Len(t, rows, 1, "seed %d", seed)
		assert.Equal(t, content.String(), rows[0].Message.Content, "seed %d", seed)
		assert.Equal(t, reasoning.String(), rows[0].Message.ReasoningContent, "seed %d", seed)
		assert.JSONEq(t, mustJSON(t, reference), mustJSON(t, rows[0].Message.Presentation), "seed %d", seed)
	}
}

func TestStreamChunkPersistedToolOpOmitsUnchangedDefinition(t *testing.T) {
	st := &streamingState{}
	def := tools.Tool{Name: "read", Category: "builtin", Description: "v1"}
	first := st.persistedToolOp(chat.AssistantPartOp{Kind: "tool_delta", ToolCallID: "c", Definition: &def})
	require.NotNil(t, first.Definition, "first definition for a call is written")
	same := st.persistedToolOp(chat.AssistantPartOp{Kind: "tool_delta", ToolCallID: "c", Arguments: "x", Definition: &def})
	assert.Nil(t, same.Definition, "an unchanged definition is not repeated for the same call")
	changed := tools.Tool{Name: "read", Category: "builtin", Description: "v2"}
	next := st.persistedToolOp(chat.AssistantPartOp{Kind: "tool_delta", ToolCallID: "c", Definition: &changed})
	assert.NotNil(t, next.Definition, "a changed definition is written again")
	other := st.persistedToolOp(chat.AssistantPartOp{Kind: "tool_delta", ToolCallID: "d", Definition: &def})
	assert.NotNil(t, other.Definition, "definitions are tracked per call")
}

func TestStreamChunkStreamingStoreAbsentKeepsFullMessagePath(t *testing.T) {
	// Hiding the chunk methods selects the per-delta UpdateMessage path.
	store := struct{ session.Store }{session.NewInMemorySessionStore()}
	sess := newStreamSession(t, store, "stream-fallback")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "x"))
	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "y"))
	require.NoError(t, persistence.completionError(sess.ID))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, streamRows(loaded), 1)
	assert.Equal(t, "xy", streamRows(loaded)[0].Message.Content)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}

// A delta from a superseded generation is dropped before persistence and
// publication; the current generation's identical delta is persisted and forwarded.
func TestObserveDropsStaleGenerationChunks(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("stale-generation"))
	require.NoError(t, r.sessionStore.AddSession(t.Context(), sess))
	d := r.sessionDrivers.Get(sess)
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.sess, d.generation = sess, 2
		return nil
	}))
	persistence := newPersistenceObserver(r.sessionStore)
	persistence.lifetime = t.Context()
	r.observers = []EventObserver{persistence}
	run := func(generation uint64, text string) []Event {
		ctx := context.WithValue(t.Context(), executionIdentityKey{}, executionIdentity{driver: d, generation: generation})
		inner := make(chan Event, 1)
		inner <- AgentChoice("root", sess.ID, text)
		close(inner)
		var got []Event
		for event := range r.observe(ctx, sess, inner) {
			got = append(got, event)
		}
		return got
	}
	require.Empty(t, run(1, "stale"), "a superseded generation is not published")
	loaded, err := r.sessionStore.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, streamRows(loaded), "and not persisted")

	require.Len(t, run(2, "live"), 1)
	loaded, err = r.sessionStore.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, "live", rows[0].Message.Content)
}

// TestStreamChunkCancelThenRetryKeepsCanceledPartialAndAnswer is the regression
// for the approved cancel compaction. Run 1 streams a partial and is canceled
// (StreamStopped reason canceled, no MessageAdded, no ErrorEvent). Run 2 is a
// Retry, so it has no user event and its deltas arrive on the same journal.
// Expected: two assistant rows, the canceled partial first, the retry answer second.
func TestStreamChunkCancelThenRetryKeepsCanceledPartialAndAnswer(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cancel-retry.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	sess := session.New(session.WithID("cancel-retry"), session.WithTitle("cancel retry"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	drive := func(events ...Event) {
		inner := make(chan Event, len(events))
		for _, event := range events {
			inner <- event
		}
		close(inner)
		for range r.observe(ctx, sess, inner) {
		}
	}

	drive(
		AgentChoice("root", sess.ID, "partial "),
		AgentChoice("root", sess.ID, "part 2"),
		StreamStopped(sess.ID, "root", turnEndReasonCanceled),
	)
	retry := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "retry answer"})
	drive(
		AgentChoice("root", sess.ID, "retry "),
		AgentChoice("root", sess.ID, "answer"),
		MessageAdded(sess.ID, retry, "root"),
		StreamStopped(sess.ID, "root", turnEndReasonNormal),
	)
	require.NoError(t, persistence.completionError(sess.ID))

	assertCancelRetryRows(t, ctx, store, path, sess.ID)
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assertCancelRetryRows(t, ctx, reopened, path, sess.ID)
	recentStore, ok := reopened.(session.RecentChatStore)
	require.True(t, ok)
	recent, err := recentStore.ReadRecentChatItems(ctx, sess.ID, 0, 0, 20, 200, 2048)
	require.NoError(t, err)
	var texts []string
	var positions []int
	for _, message := range recent.Messages {
		if message.Role == string(chat.MessageRoleAssistant) {
			texts = append(texts, message.Text)
			positions = append(positions, message.Position)
		}
	}
	assert.Equal(t, []string{"retry answer", "partial part 2"}, texts, "recent chat is newest first")
	assert.Greater(t, positions[0], positions[1], "recent chat orders by position DESC")
}

func assertCancelRetryRows(t *testing.T, ctx context.Context, store session.Store, path, sessionID string) {
	t.Helper()
	loaded, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 2)
	assert.Equal(t, "partial part 2", rows[0].Message.Content, "the canceled partial is canonical, exactly")
	assert.Equal(t, "retry answer", rows[1].Message.Content)
	assert.Less(t, rows[0].ID, rows[1].ID, "rows are ordered by position")

	summaries, err := store.GetSessionSummaries(ctx)
	require.NoError(t, err)
	var counted int
	for _, summary := range summaries {
		if summary.ID == sessionID {
			counted = summary.NumMessages
		}
	}
	assert.Equal(t, 2, counted, "catalog counts both assistant rows")

	chunks, open, closed, err := readStreamCounts(ctx, path, sessionID)
	require.NoError(t, err)
	assert.Zero(t, chunks, "no chunk rows remain")
	assert.Zero(t, open, "no open rows remain")
	assert.Equal(t, 2, closed, "both assistant rows are closed")
}

// readStreamCounts reads the stream state straight from the database file:
// chunk rows, open rows (any role), and closed assistant message rows of sessionID.
func readStreamCounts(ctx context.Context, path, sessionID string) (chunks, open, closed int, err error) {
	db, err := sqliteutil.OpenDB(ctx, path)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = sqliteutil.CloseDB(db) }()
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_message_chunks").Scan(&chunks); err != nil {
		return 0, 0, 0, err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_items WHERE stream_open = 1").Scan(&open); err != nil {
		return 0, 0, 0, err
	}
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_items WHERE session_id = ? AND stream_open = 0 AND item_type = 'message' AND json_extract(message_json, '$.role') = 'assistant'", sessionID).Scan(&closed)
	return chunks, open, closed, err
}

// Gap: a canceled StreamStopped that observe never delivered (bounded send
// timeout) leaves the row open. The driver then releases the session, which
// drops the journal; the release compacts the row off-lock.
func TestStreamChunkReleaseCompactsRowWhoseStopWasNotDelivered(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "release.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("released"), session.WithTitle("released"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	inner := make(chan Event, 2)
	inner <- AgentChoice("root", sess.ID, "partial one")
	inner <- AgentChoice("root", sess.ID, " two")
	close(inner)
	for range r.observe(ctx, sess, inner) {
	}
	require.NoError(t, persistence.completionError(sess.ID))
	_, open, _, err := readStreamCounts(ctx, path, sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, open, "before release the row is open")

	persistence.release(sess.ID)
	require.Eventually(t, func() bool {
		chunks, open, closed, err := readStreamCounts(ctx, path, sess.ID)
		return err == nil && chunks == 0 && open == 0 && closed == 1
	}, 5*time.Second, 10*time.Millisecond, "release compacts the open row")
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, "partial one two", rows[0].Message.Content)
}

// A whitespace-only turn has no tool calls and no settle event. Its normal stop
// is the terminal boundary, so the row is compacted there: one canonical row, no
// chunks, and the same after a reopen.
func TestStreamChunkBlankTurnCompactsAtNormalStop(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "blank.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	sess := session.New(session.WithID("blank-turn"), session.WithTitle("blank"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "\n"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"\n"})
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assertStreamCompacted(t, ctx, reopened, path, sess.ID, []string{"\n"})
}

// The stop is published only after its compaction committed, so an observer that
// reads the store on StreamStopped sees the settled row.
func TestStreamChunkStopCompactsBeforePublication(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "publish.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("publish-stop"), session.WithTitle("publish"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	var violations []string
	mirror := &fnObserver{onEvent: func(ctx context.Context, _ *session.Session, e Event) {
		if _, ok := e.(*StreamStoppedEvent); !ok {
			return
		}
		chunks, open, _, err := readStreamCounts(ctx, path, sess.ID)
		if err != nil || chunks != 0 || open != 0 {
			violations = append(violations, fmt.Sprintf("at stop: chunks=%d open=%d err=%v", chunks, open, err))
		}
	}}
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence, mirror}}
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "partial"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assert.Empty(t, violations)
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"partial"})
}

// A settle followed by the run's normal stop keeps the final message exactly:
// the stop finds no open stream and must not rewrite the settled row.
func TestStreamChunkSettleThenStopKeepsFinalMessage(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "settle-stop.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("settle-stop"), session.WithTitle("settle"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	final := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "final answer"})
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "draft "), MessageAdded(sess.ID, final, "root"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"final answer"})
}

// An error closes the partial with its ErrorEvent; the error stop that follows
// finds it already closed, and a Retry writes its answer as a new row.
func TestStreamChunkErrorThenRetryKeepsErroredPartialAndAnswer(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "error-retry.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("error-retry"), session.WithTitle("error retry"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "partial"), ErrorForSession(sess.ID, "boom"), StreamStopped(sess.ID, "root", turnEndReasonError))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"partial"})

	answer := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "retry answer"})
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "retry "), AgentChoice("root", sess.ID, "answer"), MessageAdded(sess.ID, answer, "root"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"partial", "retry answer"})
}

// A user cancel can reach the stream as a model error with no ErrorEvent, so its
// stop carries reason error. It must compact exactly like a canceled stop, so the
// Retry that follows writes a new row instead of continuing the canceled one.
func TestStreamChunkCanceledModelErrorCompactsAndRetryStartsNewRow(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "canceled-error.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("canceled-error"), session.WithTitle("canceled error"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "partial"), StreamStopped(sess.ID, "root", turnEndReasonError))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"partial"})

	answer := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "retry answer"})
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "retry answer"), MessageAdded(sess.ID, answer, "root"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"partial", "retry answer"})
}

// A blank first run ends at its normal stop. The next run's answer is its own
// row: nothing continues the finished blank one.
func TestStreamChunkBlankRunThenAnswerStartsNewRow(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "blank-answer.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	sess := session.New(session.WithID("blank-answer"), session.WithTitle("blank answer"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "\n"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	answer := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"})
	driveObserved(ctx, r, sess, AgentChoice("root", sess.ID, "answer"), MessageAdded(sess.ID, answer, "root"), StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	assertStreamCompacted(t, ctx, store, path, sess.ID, []string{"\n", "answer"})
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	assertStreamCompacted(t, ctx, reopened, path, sess.ID, []string{"\n", "answer"})
}

// assertStreamCompacted checks the settled state of sessionID: no chunk rows, no
// open rows, and exactly the wanted assistant contents in order through the store.
func assertStreamCompacted(t *testing.T, ctx context.Context, store session.Store, path, sessionID string, wantRows []string) {
	t.Helper()
	chunks, open, closed, err := readStreamCounts(ctx, path, sessionID)
	require.NoError(t, err)
	assert.Zero(t, chunks, "no chunk rows remain")
	assert.Zero(t, open, "no open rows remain")
	assert.Equal(t, len(wantRows), closed, "every assistant row is closed")
	loaded, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	var contents []string
	for _, row := range streamRows(loaded) {
		contents = append(contents, row.Message.Content)
	}
	assert.Equal(t, wantRows, contents)
}

// Known open path: invalidateTranscript is consumed lazily, so with no further
// event the stream row stays open; the next event compacts it and starts fresh.
func TestStreamChunkInvalidateCompactsOnNextEvent(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "invalidate.db")
	store, err := sqlitestore.New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("invalidate"), session.WithTitle("invalidate"))
	require.NoError(t, store.AddSession(ctx, sess))
	persistence := newPersistenceObserver(store)
	persistence.lifetime = ctx
	persistence.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "before edit"))
	require.NoError(t, persistence.completionError(sess.ID))
	persistence.invalidateTranscript(sess.ID)
	_, open, _, err := readStreamCounts(ctx, path, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, open, "invalidation alone does not write")

	persistence.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "after edit"))
	require.NoError(t, persistence.completionError(sess.ID))
	// The old row is compacted; "after edit" is a new, still-streaming row.
	chunks, open, closed, err := readStreamCounts(ctx, path, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, chunks)
	assert.Equal(t, 1, open)
	assert.Equal(t, 1, closed)
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 2)
	assert.Equal(t, "before edit", rows[0].Message.Content)
	assert.Equal(t, "after edit", rows[1].Message.Content)

	persistence.OnEvent(ctx, sess, UserMessage("next", sess.ID, nil))
	require.NoError(t, persistence.completionError(sess.ID))
	chunks, open, closed, err = readStreamCounts(ctx, path, sess.ID)
	require.NoError(t, err)
	assert.Zero(t, chunks)
	assert.Zero(t, open)
	assert.Equal(t, 2, closed)
}

// TestStreamChunkGoldenPresentationRow pins the stored presentation of one fixed
// stream, so a reducer change cannot silently reshape persisted rows.
func TestStreamChunkGoldenPresentationRow(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-golden")
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	def := tools.Tool{Name: "read", Category: "builtin"}
	partial := func(arguments string) Event {
		return PartialToolCall(tools.ToolCall{ID: "c1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: arguments}}, def, "root")
	}
	persistence.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "a"))
	persistence.OnEvent(t.Context(), sess, partial(`{"p`))
	persistence.OnEvent(t.Context(), sess, partial(`":1}`))
	persistence.OnEvent(t.Context(), sess, ToolCallResponse("c1", def, tools.ResultSuccess("ok"), "ok", "root"))
	persistence.OnEvent(t.Context(), sess, StreamStopped(sess.ID, "root", turnEndReasonNormal))
	require.NoError(t, persistence.completionError(sess.ID))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	answer := "ok"
	want := []chat.AssistantPart{
		{Type: chat.AssistantPartContent, Text: "a"},
		{Type: chat.AssistantPartToolCall, ToolCallID: "c1", Tool: &chat.AssistantTool{
			Call:       tools.ToolCall{ID: "c1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{"p":1}`}},
			Definition: def,
			Result:     &answer,
		}},
	}
	assert.JSONEq(t, mustJSON(t, want), mustJSON(t, rows[0].Message.Presentation))
}

// baselineCaptureAssistantPresentation is the tool presentation reducer at
// a289ea8d3, before it moved to chat.ApplyAssistantPartOp. It is kept verbatim as
// the independent reference that stored rows are compared against.
func baselineCaptureAssistantPresentation(parts []chat.AssistantPart, event Event) []chat.AssistantPart {
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
