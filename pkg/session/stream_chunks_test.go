package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

const streamTestAgent = "streamer"

func streamingStores(t *testing.T) map[string]Store {
	t.Helper()
	return map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)}
}

func streamingOf(t *testing.T, store Store) StreamingMessageStore {
	t.Helper()
	streaming, ok := store.(StreamingMessageStore)
	require.True(t, ok)
	return streaming
}

func textChunk(kind MessageChunkKind, text string) MessageChunk {
	return MessageChunk{Kind: kind, Text: text}
}

func toolChunk(op chat.AssistantPartOp) MessageChunk {
	return MessageChunk{Kind: MessageChunkTool, Op: &op}
}

// streamCatalog reads the catalog counters the 040 triggers maintain (SQLite only).
func streamCatalog(t *testing.T, store Store, sessionID string) (numMessages, itemMessages int, cost float64) {
	t.Helper()
	sqlStore, ok := store.(*SQLiteSessionStore)
	require.True(t, ok)
	require.NoError(t, sqlStore.db.QueryRowContext(t.Context(), `SELECT catalog_num_messages, catalog_item_messages, catalog_cost FROM sessions WHERE id = ?`, sessionID).Scan(&numMessages, &itemMessages, &cost))
	return numMessages, itemMessages, cost
}

func summaryMessageCount(t *testing.T, store Store, sessionID string) int {
	t.Helper()
	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	for _, summary := range summaries {
		if summary.ID == sessionID {
			return summary.NumMessages
		}
	}
	t.Fatalf("missing summary for %s", sessionID)
	return 0
}

func onlyAssistantMessage(t *testing.T, store Store, sessionID string) *Message {
	t.Helper()
	sess, err := store.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, sess.Messages, 1)
	require.NotNil(t, sess.Messages[0].Message)
	return sess.Messages[0].Message
}

func streamedMessageJSON(t *testing.T, msg *Message) string {
	t.Helper()
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	return string(data)
}

func TestReconstructStreamingMessageMatchesPartialRow(t *testing.T) {
	t.Parallel()
	empty := ReconstructStreamingMessage("a", nil)
	assert.Equal(t, Message{AgentName: "a", Message: chat.Message{Role: chat.MessageRoleAssistant}}, empty)
	assert.Nil(t, empty.Message.Presentation)

	got := ReconstructStreamingMessage("a", []MessageChunk{
		textChunk(MessageChunkReasoning, "think"),
		textChunk(MessageChunkContent, "Hello "),
		textChunk(MessageChunkContent, "world"),
		toolChunk(chat.AssistantPartOp{Kind: chat.AssistantPartOpToolDelta, ToolCallID: "t1", Name: "fn", Arguments: "{}"}),
		textChunk(MessageChunkContent, "!"),
	})
	assert.Equal(t, "Hello world!", got.Message.Content)
	assert.Equal(t, "think", got.Message.ReasoningContent)
	assert.Equal(t, []chat.AssistantPart{
		{Type: chat.AssistantPartReasoning, Text: "think"},
		{Type: chat.AssistantPartContent, Text: "Hello world"},
		{Type: chat.AssistantPartToolCall, ToolCallID: "t1", Tool: &chat.AssistantTool{Call: tools.ToolCall{ID: "t1", Function: tools.FunctionCall{Name: "fn", Arguments: "{}"}}}},
		{Type: chat.AssistantPartContent, Text: "!"},
	}, got.Message.Presentation)
}

// legacyEvent models the runtime events that built full partial rows before chunks.
type legacyEvent struct {
	kind       string // content, reasoning, partial, call, response
	text       string
	call       tools.ToolCall
	definition *tools.Tool
	result     *string
	isError    bool
}

// legacyCapture is a local copy of the runtime's capturePresentation and its tool
// branch, the algorithm whose full-JSON output the chunk replay must reproduce.
func legacyCapture(text *chat.AssistantTextAppender, parts []chat.AssistantPart, e legacyEvent) []chat.AssistantPart {
	switch e.kind {
	case "content":
		return text.Append(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: e.text})
	case "reasoning":
		return text.Append(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: e.text})
	}
	var call tools.ToolCall
	var definition *tools.Tool
	var result *string
	var isError, delta bool
	switch e.kind {
	case "partial":
		call, definition, delta = e.call, e.definition, true
	case "call":
		call, definition = e.call, e.definition
	case "response":
		call.ID, definition, result, isError = e.call.ID, e.definition, e.result, e.isError
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
		tool.Definition = *definition
	}
	return parts
}

// legacyChunk maps an event to the chunk the runtime persists for it.
func legacyChunk(e legacyEvent) MessageChunk {
	switch e.kind {
	case "content":
		return textChunk(MessageChunkContent, e.text)
	case "reasoning":
		return textChunk(MessageChunkReasoning, e.text)
	case "partial":
		call := e.call
		return toolChunk(chat.AssistantPartOp{Kind: chat.AssistantPartOpToolDelta, ToolCallID: call.ID, Type: string(call.Type), Name: call.Function.Name, Arguments: call.Function.Arguments, Definition: e.definition})
	case "call":
		call := e.call
		return toolChunk(chat.AssistantPartOp{Kind: chat.AssistantPartOpToolCall, ToolCallID: call.ID, Call: &call, Definition: e.definition})
	default:
		return toolChunk(chat.AssistantPartOp{Kind: chat.AssistantPartOpToolResult, ToolCallID: e.call.ID, Result: e.result, IsError: e.isError})
	}
}

func randomLegacyEvent(rng *rand.Rand) legacyEvent {
	ids := []string{"t1", "t2", "t3"}
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	definition := func() *tools.Tool {
		return &tools.Tool{Name: pick([]string{"fn_a", "fn_b"}), Description: pick([]string{"", "desc é"}), Category: "cat"}
	}
	switch rng.Intn(8) {
	case 0, 1:
		return legacyEvent{kind: "content", text: pick([]string{"", "héllo ", "wörld", "日本"})}
	case 2:
		return legacyEvent{kind: "reasoning", text: pick([]string{"", "hmm ", "ok"})}
	case 3, 4:
		var def *tools.Tool
		if rng.Intn(2) == 0 {
			def = definition()
		}
		call := tools.ToolCall{ID: pick(ids), Type: tools.ToolType(pick([]string{"", "function"})), Function: tools.FunctionCall{Name: pick([]string{"", "fn_a", "fn_b"}), Arguments: pick([]string{"", "{", "\"k\":", "1}"})}}
		return legacyEvent{kind: "partial", call: call, definition: def}
	case 5:
		call := tools.ToolCall{ID: pick(ids), Type: "function", Function: tools.FunctionCall{Name: "fn_a", Arguments: pick([]string{"", "{\"k\":1}"})}}
		return legacyEvent{kind: "call", call: call, definition: definition()}
	default:
		result := pick([]string{"", "ok", "boom"})
		return legacyEvent{kind: "response", call: tools.ToolCall{ID: pick(ids)}, definition: definition(), result: &result, isError: rng.Intn(2) == 0}
	}
}

// buildLegacyMessage returns the full message the runtime wrote before chunks existed.
func buildLegacyMessage(events []legacyEvent) *Message {
	var text chat.AssistantTextAppender
	var parts []chat.AssistantPart
	var content, reasoning strings.Builder
	for _, e := range events {
		switch e.kind {
		case "content":
			content.WriteString(e.text)
		case "reasoning":
			reasoning.WriteString(e.text)
		}
		parts = legacyCapture(&text, parts, e)
	}
	return &Message{AgentName: streamTestAgent, Message: chat.Message{
		Role: chat.MessageRoleAssistant, Content: content.String(), ReasoningContent: reasoning.String(), Presentation: parts,
	}}
}

func TestStreamedRowsReplayToLegacyMessageBytes(t *testing.T) {
	t.Parallel()
	for seed := range 60 {
		rng := rand.New(rand.NewSource(int64(seed)))
		events := make([]legacyEvent, 1+rng.Intn(60))
		chunks := make([]MessageChunk, len(events))
		for i := range events {
			events[i] = randomLegacyEvent(rng)
			chunks[i] = legacyChunk(events[i])
		}
		want := streamedMessageJSON(t, buildLegacyMessage(events))
		for name, store := range streamingStores(t) {
			streaming := streamingOf(t, store)
			sess := New(WithID(fmt.Sprintf("parity-%d", seed)))
			require.NoError(t, store.AddSession(t.Context(), sess))
			first := 1 + rng.Intn(len(chunks))
			itemID, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "w", AgentName: streamTestAgent}, 0, 0, chunks[:first])
			require.NoError(t, err)
			for next := first; next < len(chunks); {
				batch := min(len(chunks)-next, 1+rng.Intn(5))
				_, err := streaming.AppendMessageChunks(t.Context(), sess.ID, nil, itemID, int64(next), chunks[next:next+batch])
				require.NoError(t, err, "%s seed %d", name, seed)
				next += batch
			}
			assert.Equal(t, want, streamedMessageJSON(t, onlyAssistantMessage(t, store, sess.ID)), "%s seed %d (open row)", name, seed)
			require.NoError(t, streaming.CloseOpenStreams(t.Context(), sess.ID))
			assert.Equal(t, want, streamedMessageJSON(t, onlyAssistantMessage(t, store, sess.ID)), "%s seed %d (compacted row)", name, seed)
		}
	}
}

func TestStreamedRecentChatDisplayMatchesCanonicalRow(t *testing.T) {
	t.Parallel()
	for seed := range 40 {
		rng := rand.New(rand.NewSource(int64(1000 + seed)))
		events := make([]legacyEvent, 1+rng.Intn(80))
		for i := range events {
			events[i] = randomLegacyEvent(rng)
			// Long multi-byte content runs force the 16-part and byte truncation branches.
			if rng.Intn(3) == 0 {
				events[i] = legacyEvent{kind: "content", text: strings.Repeat("é", rng.Intn(12)) + "x"}
			}
		}
		chunks := make([]MessageChunk, len(events))
		for i, event := range events {
			chunks[i] = legacyChunk(event)
		}
		for name, store := range streamingStores(t) {
			streaming := streamingOf(t, store)
			reader, ok := store.(RecentChatStore)
			require.True(t, ok)
			for _, maxText := range []int{1, 7, 16, 40, 16 * 1024} {
				sess := New(WithID(fmt.Sprintf("recent-%d-%d", seed, maxText)))
				require.NoError(t, store.AddSession(t.Context(), sess))
				itemID, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "w", AgentName: streamTestAgent}, 0, 0, chunks)
				require.NoError(t, err)
				openPage, err := reader.ReadRecentChatItems(t.Context(), sess.ID, 0, 0, 5, 10, maxText)
				require.NoError(t, err)
				require.Len(t, openPage.Messages, 1)
				require.Equal(t, itemID, openPage.Messages[0].ID)
				require.NoError(t, streaming.CloseOpenStreams(t.Context(), sess.ID))
				closedPage, err := reader.ReadRecentChatItems(t.Context(), sess.ID, 0, 0, 5, 10, maxText)
				require.NoError(t, err)
				assert.Equal(t, closedPage, openPage, "%s seed %d max %d", name, seed, maxText)
			}
		}
	}
}

func TestStreamingPlaceholderIdempotentRetry(t *testing.T) {
	t.Parallel()
	start := &StreamStart{WriteID: "write-1", AgentName: streamTestAgent}
	chunks := []MessageChunk{textChunk(MessageChunkContent, "hi")}
	for name, store := range streamingStores(t) {
		t.Run(name, func(t *testing.T) {
			streaming := streamingOf(t, store)
			sess := New(WithID("retry"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			id, err := streaming.AppendMessageChunks(t.Context(), sess.ID, start, 0, 0, chunks)
			require.NoError(t, err)
			again, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "write-1", AgentName: streamTestAgent}, 0, 0, chunks)
			require.NoError(t, err)
			assert.Equal(t, id, again, "same writeID and payload returns the same row")
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, start, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "other")})
			require.ErrorIs(t, err, ErrWriteConflict)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "write-1", AgentName: "someone-else"}, 0, 0, chunks)
			require.ErrorIs(t, err, ErrWriteConflict)

			// Two identical streams with distinct write IDs are two rows.
			twin, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "write-2", AgentName: streamTestAgent}, 0, 0, chunks)
			require.NoError(t, err)
			assert.NotEqual(t, id, twin)
			numMessages, itemMessages, _ := streamCatalogOrZero(t, store, sess.ID)
			assert.Equal(t, 2, summaryMessageCount(t, store, sess.ID))
			if name == "sqlite" {
				assert.Equal(t, 2, numMessages)
				assert.Equal(t, 2, itemMessages)
			}

			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "write-3", AgentName: streamTestAgent}, 0, 0, chunks)
			require.NoError(t, err)
			_, err = streaming.AppendMessageChunks(t.Context(), "missing-session", &StreamStart{WriteID: "x", AgentName: streamTestAgent}, 0, 0, chunks)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

// streamCatalogOrZero returns catalog counters for SQLite and zeros for the in-memory store.
func streamCatalogOrZero(t *testing.T, store Store, sessionID string) (int, int, float64) {
	t.Helper()
	if _, ok := store.(*SQLiteSessionStore); !ok {
		return 0, 0, 0
	}
	return streamCatalog(t, store, sessionID)
}

func TestStreamingPlaceholderRetryAfterRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stream-restart.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	sess := New(WithID("restart"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	start := &StreamStart{WriteID: "durable", AgentName: streamTestAgent}
	chunks := []MessageChunk{textChunk(MessageChunkReasoning, "why"), textChunk(MessageChunkContent, "because")}
	id, err := streamingOf(t, store).AppendMessageChunks(t.Context(), sess.ID, start, 0, 0, chunks)
	require.NoError(t, err)
	require.NoError(t, store.(*SQLiteSessionStore).Close())

	reopened, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	retry, err := streamingOf(t, reopened).AppendMessageChunks(t.Context(), sess.ID, start, 0, 0, chunks)
	require.NoError(t, err)
	assert.Equal(t, id, retry)
	assert.Equal(t, 1, summaryMessageCount(t, reopened, sess.ID))
}

func TestStreamingChunkAppendSequenceRules(t *testing.T) {
	t.Parallel()
	for name, store := range streamingStores(t) {
		t.Run(name, func(t *testing.T) {
			streaming := streamingOf(t, store)
			sess := New(WithID("seq"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			id, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "s", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "a")})
			require.NoError(t, err)
			batch := []MessageChunk{textChunk(MessageChunkContent, "b"), textChunk(MessageChunkContent, "c")}
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 1, batch)
			require.NoError(t, err)
			// Ambiguous commit: the same batch is retried after it landed.
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 1, batch)
			require.NoError(t, err)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 5, batch)
			require.ErrorIs(t, err, ErrWriteConflict, "gap")
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 2, batch)
			require.ErrorIs(t, err, ErrWriteConflict, "partial overlap")
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id+999, 3, batch)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 3, []MessageChunk{{Kind: "bogus"}})
			require.Error(t, err)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 3, []MessageChunk{{Kind: MessageChunkTool}})
			require.Error(t, err)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 3, []MessageChunk{textChunk(MessageChunkContent, "d")})
			require.NoError(t, err)
			assert.Equal(t, "abcd", onlyAssistantMessage(t, store, sess.ID).Message.Content)
		})
	}
}

func TestStreamingCloseSemantics(t *testing.T) {
	t.Parallel()
	for name, store := range streamingStores(t) {
		t.Run(name, func(t *testing.T) {
			streaming := streamingOf(t, store)
			sess := New(WithID("close"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			id, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "c", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "partial")})
			require.NoError(t, err)

			final := &Message{AgentName: streamTestAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "final answer", Cost: 0.5}}
			require.NoError(t, streaming.CloseStreamingMessage(t.Context(), sess.ID, id, final))
			assert.Equal(t, "final answer", onlyAssistantMessage(t, store, sess.ID).Message.Content)
			require.ErrorIs(t, streaming.CloseStreamingMessage(t.Context(), sess.ID, id, final), ErrStreamClosed)
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 1, []MessageChunk{textChunk(MessageChunkContent, "late")})
			require.ErrorIs(t, err, ErrStreamClosed)
			require.ErrorIs(t, streaming.CloseStreamingMessage(t.Context(), sess.ID, id+999, final), ErrNotFound)
			require.ErrorIs(t, streaming.CloseStreamingMessage(t.Context(), "missing", id, final), ErrNotFound)
			assert.Equal(t, "final answer", onlyAssistantMessage(t, store, sess.ID).Message.Content)
		})
	}
}

func TestStreamingCloseWithoutFinalCompactsChunks(t *testing.T) {
	t.Parallel()
	for name, store := range streamingStores(t) {
		t.Run(name, func(t *testing.T) {
			streaming := streamingOf(t, store)
			sess := New(WithID("compact"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			chunks := []MessageChunk{textChunk(MessageChunkContent, "stopped "), textChunk(MessageChunkContent, "early")}
			id, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "k", AgentName: streamTestAgent}, 0, 0, chunks)
			require.NoError(t, err)
			require.NoError(t, streaming.CloseStreamingMessage(t.Context(), sess.ID, id, nil))
			msg := onlyAssistantMessage(t, store, sess.ID)
			assert.Equal(t, "stopped early", msg.Message.Content)
			assert.Equal(t, streamedMessageJSON(t, &Message{AgentName: streamTestAgent, Message: ReconstructStreamingMessage(streamTestAgent, chunks).Message}), streamedMessageJSON(t, msg))
			require.ErrorIs(t, streaming.CloseStreamingMessage(t.Context(), sess.ID, id, nil), ErrStreamClosed)
		})
	}
}

func TestStreamingEditClosesStreamAndRejectsLaterChunks(t *testing.T) {
	t.Parallel()
	for name, store := range streamingStores(t) {
		t.Run(name, func(t *testing.T) {
			streaming := streamingOf(t, store)
			sess := New(WithID("edit"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			id, err := streaming.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "e", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "streamed")})
			require.NoError(t, err)
			edited := &Message{AgentName: streamTestAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "edited"}}
			require.NoError(t, store.UpdateMessage(t.Context(), sess.ID, id, edited))
			_, err = streaming.AppendMessageChunks(t.Context(), sess.ID, nil, id, 1, []MessageChunk{textChunk(MessageChunkContent, " more")})
			require.ErrorIs(t, err, ErrStreamClosed)
			assert.Equal(t, "edited", onlyAssistantMessage(t, store, sess.ID).Message.Content)
			require.NoError(t, streaming.CloseOpenStreams(t.Context(), sess.ID))
			assert.Equal(t, "edited", onlyAssistantMessage(t, store, sess.ID).Message.Content)
		})
	}
}

func TestStreamingStartIsAtomicOnFailure(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("atomic"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	// startStreamingMessage bypasses validation, so the CHECK constraint on the chunk
	// kind fails after the placeholder insert. The whole transaction must roll back.
	_, err := store.startStreamingMessage(t.Context(), sess.ID, StreamStart{WriteID: "atomic", AgentName: streamTestAgent}, []MessageChunk{{Kind: "bogus"}})
	require.Error(t, err)
	var items, receipts, chunks int
	require.NoError(t, store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items WHERE session_id = ?`, sess.ID).Scan(&items))
	require.NoError(t, store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_append_receipts WHERE session_id = ?`, sess.ID).Scan(&receipts))
	require.NoError(t, store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_message_chunks`).Scan(&chunks))
	assert.Zero(t, items)
	assert.Zero(t, receipts)
	assert.Zero(t, chunks)
	// The write ID stays free for a correct retry.
	_, err = store.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "atomic", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "ok")})
	require.NoError(t, err)
}

func TestStreamingCatalogCountsDuringAndAfterStream(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("counts"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	id, err := store.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "n", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "a")})
	require.NoError(t, err)
	_, err = store.AppendMessageChunks(t.Context(), sess.ID, nil, id, 1, []MessageChunk{textChunk(MessageChunkContent, "b")})
	require.NoError(t, err)
	num, items, cost := streamCatalog(t, store, sess.ID)
	assert.Equal(t, 1, num, "placeholder counts once while open")
	assert.Equal(t, 1, items)
	assert.InDelta(t, 0, cost, 1e-9)
	assert.Equal(t, 1, summaryMessageCount(t, store, sess.ID))

	require.NoError(t, store.CloseStreamingMessage(t.Context(), sess.ID, id, &Message{AgentName: streamTestAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "ab", Cost: 0.25}}))
	num, items, cost = streamCatalog(t, store, sess.ID)
	assert.Equal(t, 1, num)
	assert.Equal(t, 1, items)
	assert.InDelta(t, 0.25, cost, 1e-9)
}

func TestStreamingCascadeDeletesChunks(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("cascade"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	id, err := store.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "x", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "a"), textChunk(MessageChunkReasoning, "b")})
	require.NoError(t, err)
	countChunks := func() int {
		var n int
		require.NoError(t, store.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_message_chunks WHERE item_id = ?`, id).Scan(&n))
		return n
	}
	require.Equal(t, 2, countChunks())
	_, err = store.db.ExecContext(t.Context(), `DELETE FROM session_items WHERE id = ?`, id)
	require.NoError(t, err)
	assert.Zero(t, countChunks(), "deleting the row cascades to its chunks")

	id, err = store.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "y", AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "c")})
	require.NoError(t, err)
	require.NoError(t, store.DeleteSession(t.Context(), sess.ID))
	assert.Zero(t, countChunks(), "deleting the session cascades to its chunks")
}

func TestStreamingCrashMidStreamRecovers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "crash.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	sess := New(WithID("crash"), WithUserMessage("question"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	chunks := []MessageChunk{
		textChunk(MessageChunkReasoning, "thinking"),
		textChunk(MessageChunkContent, "partial "),
		toolChunk(chat.AssistantPartOp{Kind: chat.AssistantPartOpToolDelta, ToolCallID: "t1", Name: "lookup", Arguments: "{\"q\""}),
		textChunk(MessageChunkContent, "answer"),
	}
	_, err = streamingOf(t, store).AppendMessageChunks(t.Context(), sess.ID, &StreamStart{WriteID: "crash-write", AgentName: streamTestAgent}, 0, 0, chunks)
	require.NoError(t, err)
	require.NoError(t, store.(*SQLiteSessionStore).Close())

	reopened, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	want := ReconstructStreamingMessage(streamTestAgent, chunks)
	loaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 2)
	assert.Equal(t, streamedMessageJSON(t, &Message{AgentName: streamTestAgent, Message: want.Message}), streamedMessageJSON(t, loaded.Messages[1].Message))

	reader := reopened.(RecentChatStore)
	page, err := reader.ReadRecentChatItems(t.Context(), sess.ID, 0, 0, 10, 10, 1024)
	require.NoError(t, err)
	require.Len(t, page.Messages, 2)
	assert.Equal(t, string(chat.MessageRoleAssistant), page.Messages[0].Role)
	assert.Equal(t, "partial answer", page.Messages[0].Text)
	numBefore, itemsBefore, costBefore := streamCatalog(t, reopened, sess.ID)

	require.NoError(t, reopened.(StreamingMessageStore).CloseOpenStreams(t.Context(), sess.ID))
	numAfter, itemsAfter, costAfter := streamCatalog(t, reopened, sess.ID)
	assert.Equal(t, [3]any{numBefore, itemsBefore, costBefore}, [3]any{numAfter, itemsAfter, costAfter})
	sqlStore := reopened.(*SQLiteSessionStore)
	var rows, open, chunkRows int
	require.NoError(t, sqlStore.db.QueryRowContext(t.Context(), `SELECT COUNT(*), COALESCE(SUM(stream_open), 0) FROM session_items WHERE session_id = ?`, sess.ID).Scan(&rows, &open))
	require.NoError(t, sqlStore.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_message_chunks`).Scan(&chunkRows))
	assert.Equal(t, 2, rows)
	assert.Zero(t, open)
	assert.Zero(t, chunkRows)

	loaded, err = reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, streamedMessageJSON(t, &Message{AgentName: streamTestAgent, Message: want.Message}), streamedMessageJSON(t, loaded.Messages[1].Message))
	require.NoError(t, reopened.(StreamingMessageStore).CloseOpenStreams(t.Context(), sess.ID))
}

func TestStreamingStartRejectsMissingSessionAndEmptyWriteID(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("missing-session-check"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	_, err := store.AppendMessageChunks(t.Context(), "nope", &StreamStart{WriteID: "w", AgentName: streamTestAgent}, 0, 0, nil)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = store.AppendMessageChunks(t.Context(), sess.ID, &StreamStart{}, 0, 0, nil)
	require.ErrorIs(t, err, ErrEmptyID)
}

func TestMigration042FreshDatabase(t *testing.T) {
	t.Parallel()
	db := openRawSQLiteForMigrations(t)
	require.NoError(t, NewMigrationManager(db).InitializeMigrations(t.Context()))
	var maxVersion int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT MAX(id) FROM migrations`).Scan(&maxVersion))
	assert.Equal(t, 44, maxVersion)
	assertStreamSchema(t, db, true)
}

func TestMigration042FromPreviousWithData(t *testing.T) {
	t.Parallel()
	db := openRawSQLiteForMigrations(t)
	var upTo41 []Migration
	for _, m := range getAllMigrations() {
		if m.ID <= 41 {
			upTo41 = append(upTo41, m)
		}
	}
	require.NoError(t, NewMigrationManagerWithMigrations(db, upTo41).InitializeMigrations(t.Context()))
	assertStreamSchema(t, db, false)

	// Rows written under 041 must survive the upgrade unchanged.
	legacy := &SQLiteSessionStore{db: db}
	sess := New(WithID("legacy"))
	require.NoError(t, legacy.AddSession(t.Context(), sess))
	_, err := legacy.AddMessage(t.Context(), sess.ID, NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "old answer", Cost: 0.75}))
	require.NoError(t, err)
	var numBefore, itemsBefore int
	var costBefore float64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT catalog_num_messages, catalog_item_messages, catalog_cost FROM sessions WHERE id = ?`, sess.ID).Scan(&numBefore, &itemsBefore, &costBefore))

	require.NoError(t, NewMigrationManager(db).InitializeMigrations(t.Context()))
	assertStreamSchema(t, db, true)
	var open int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_items WHERE stream_open = 1`).Scan(&open))
	assert.Zero(t, open)
	var numAfter, itemsAfter int
	var costAfter float64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT catalog_num_messages, catalog_item_messages, catalog_cost FROM sessions WHERE id = ?`, sess.ID).Scan(&numAfter, &itemsAfter, &costAfter))
	assert.Equal(t, [3]any{numBefore, itemsBefore, costBefore}, [3]any{numAfter, itemsAfter, costAfter})
	loaded, err := (&SQLiteSessionStore{db: db}).GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	assert.Equal(t, "old answer", loaded.Messages[0].Message.Message.Content)

	// Down removes the schema additions exactly.
	var down string
	for _, m := range getAllMigrations() {
		if m.ID == 42 {
			down = m.DownSQL
		}
	}
	_, err = db.ExecContext(t.Context(), down)
	require.NoError(t, err)
	assertStreamSchema(t, db, false)
}

func openRawSQLiteForMigrations(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// A single connection keeps the private in-memory database alive for the whole test.
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sessions (id TEXT PRIMARY KEY, messages TEXT, created_at TEXT)`)
	require.NoError(t, err)
	return db
}

func assertStreamSchema(t *testing.T, db *sql.DB, present bool) {
	t.Helper()
	var column int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('session_items') WHERE name = 'stream_open'`).Scan(&column))
	var table, index int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_message_chunks'`).Scan(&table))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_session_items_stream_open'`).Scan(&index))
	if present {
		assert.Equal(t, 1, column)
		assert.Equal(t, 1, table)
		assert.Equal(t, 1, index)
		return
	}
	assert.Zero(t, column)
	assert.Zero(t, table)
	assert.Zero(t, index)
}

// TestOpenStreamRowReconcilesSettleBetweenScanAndChunkRead covers the race where the
// scan saw an open row but a settle commits before, or during, the chunk read.
// Without the re-read the row would replay zero chunks and show an empty message.
func TestOpenStreamRowReconcilesSettleBetweenScanAndChunkRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	final := &Message{AgentName: streamTestAgent, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "final"}}
	open := func(t *testing.T, name string) (*SQLiteSessionStore, *Session, int64) {
		t.Helper()
		store := openMemoryStore(t)
		sess := New(WithID(name))
		require.NoError(t, store.AddSession(ctx, sess))
		id, err := store.AppendMessageChunks(ctx, sess.ID, &StreamStart{WriteID: name, AgentName: streamTestAgent}, 0, 0, []MessageChunk{textChunk(MessageChunkContent, "partial")})
		require.NoError(t, err)
		return store, sess, id
	}
	scanned := func(id int64) *sessionItemRow {
		return &sessionItemRow{id: id, agentName: sql.NullString{String: streamTestAgent, Valid: true}, streamOpen: true}
	}
	messageContent := func(t *testing.T, row *sessionItemRow) string {
		t.Helper()
		var msg chat.Message
		require.NoError(t, json.Unmarshal([]byte(row.messageJSON.String), &msg))
		return msg.Content
	}

	t.Run("settled before chunk read", func(t *testing.T) {
		t.Parallel()
		store, sess, id := open(t, "race-before")
		row := scanned(id)
		found, err := store.reconcileOpenRow(ctx, store.db, row, func() {
			require.NoError(t, store.CloseStreamingMessage(ctx, sess.ID, id, final))
		}, nil)
		require.NoError(t, err)
		require.True(t, found)
		assert.False(t, row.streamOpen)
		assert.Equal(t, "final", messageContent(t, row))
	})

	t.Run("settled between chunk read and re-read", func(t *testing.T) {
		t.Parallel()
		store, sess, id := open(t, "race-after")
		row := scanned(id)
		found, err := store.reconcileOpenRow(ctx, store.db, row, nil, func() {
			require.NoError(t, store.CloseStreamingMessage(ctx, sess.ID, id, final))
		})
		require.NoError(t, err)
		require.True(t, found)
		assert.False(t, row.streamOpen)
		assert.Equal(t, "final", messageContent(t, row))
	})

	t.Run("compacted before chunk read", func(t *testing.T) {
		t.Parallel()
		store, sess, id := open(t, "race-compact")
		row := scanned(id)
		found, err := store.reconcileOpenRow(ctx, store.db, row, func() {
			require.NoError(t, store.CloseStreamingMessage(ctx, sess.ID, id, nil))
		}, nil)
		require.NoError(t, err)
		require.True(t, found)
		assert.False(t, row.streamOpen)
		assert.Equal(t, "partial", messageContent(t, row))
	})

	t.Run("still open", func(t *testing.T) {
		t.Parallel()
		store, _, id := open(t, "race-open")
		row := scanned(id)
		found, err := store.reconcileOpenRow(ctx, store.db, row, nil, nil)
		require.NoError(t, err)
		require.True(t, found)
		assert.True(t, row.streamOpen)
		assert.Equal(t, "partial", row.openBody.Content)
	})

	t.Run("deleted between scan and chunk read", func(t *testing.T) {
		t.Parallel()
		store, _, id := open(t, "race-deleted")
		row := scanned(id)
		found, err := store.reconcileOpenRow(ctx, store.db, row, func() {
			_, err := store.db.ExecContext(ctx, `DELETE FROM session_items WHERE id = ?`, id)
			require.NoError(t, err)
		}, nil)
		require.NoError(t, err)
		assert.False(t, found)
	})
}
