package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
)

// MessageChunkKind names the presentation effect of one stored chunk.
type MessageChunkKind string

const (
	MessageChunkContent   MessageChunkKind = "content"
	MessageChunkReasoning MessageChunkKind = "reasoning"
	MessageChunkTool      MessageChunkKind = "tool"
)

// MessageChunk is one durable delta of an in-progress assistant message. Text
// applies to content and reasoning chunks; Op applies to tool chunks.
type MessageChunk struct {
	Kind MessageChunkKind
	Text string
	Op   *chat.AssistantPartOp
}

// StreamStart describes the placeholder row that opens a streamed message.
// WriteID makes the placeholder append retry-safe across restarts and ambiguous commits.
type StreamStart struct {
	WriteID   string
	AgentName string
	// MessageID is the assistant message identity kept on the open row and its canonical settle.
	MessageID string
}

// ErrStreamClosed reports a chunk append or close against a message whose stream
// already settled, was compacted, or was replaced by an edit. Callers should stop
// sending chunks for that stream; the stored row is authoritative.
var ErrStreamClosed = errors.New("session: streaming message already closed")

// StreamingMessageStore persists assistant messages incrementally. Open rows
// (stream_open=1) hold a placeholder message; their body is reconstructed from
// ordered chunks on read. Raw SQL over session_items sees the placeholder; see
// the migration 042 note in migrations.go.
type StreamingMessageStore interface {
	// AppendMessageChunks with start != nil inserts the placeholder row, its append
	// receipt and chunks seq 0..n-1 in one transaction and returns the row ID. A retry with
	// the same WriteID and identical payload returns the same ID; a different payload
	// returns ErrWriteConflict. With start == nil it appends chunks firstSeq.. to the open
	// row itemID in one transaction. A batch already landed returns success. A gap or partial
	// overlap returns ErrWriteConflict, a missing row ErrNotFound and a closed row ErrStreamClosed.
	AppendMessageChunks(ctx context.Context, sessionID string, start *StreamStart, itemID, firstSeq int64, chunks []MessageChunk) (int64, error)
	// CloseStreamingMessage settles an open row in one transaction and deletes its chunks.
	// With msg != nil it writes msg with the same columns as UpdateMessage. With msg == nil it
	// writes the message reconstructed from the chunks. An already closed row returns ErrStreamClosed.
	CloseStreamingMessage(ctx context.Context, sessionID string, itemID int64, msg *Message) error
	// CloseOpenStreams compacts every open row of sessionID in one transaction. Callers must
	// hold exclusive session ownership, because an open row from a live writer would be lost.
	CloseOpenStreams(ctx context.Context, sessionID string) error
}

// ReconstructStreamingMessage replays chunks in order into the canonical assistant message.
// It is the single reducer behind readers and close paths, and it matches the runtime's
// presentation construction. Presentation is nil when no part was produced.
func ReconstructStreamingMessage(agentName string, chunks []MessageChunk) Message {
	var content, reasoning strings.Builder
	var parts []chat.AssistantPart
	var appender chat.AssistantTextAppender
	for _, chunk := range chunks {
		switch chunk.Kind {
		case MessageChunkContent:
			content.WriteString(chunk.Text)
			parts = appender.Append(parts, chat.AssistantPart{Type: chat.AssistantPartContent, Text: chunk.Text})
		case MessageChunkReasoning:
			reasoning.WriteString(chunk.Text)
			parts = appender.Append(parts, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: chunk.Text})
		case MessageChunkTool:
			if chunk.Op != nil {
				parts = chat.ApplyAssistantPartOp(parts, *chunk.Op)
			}
		}
	}
	return Message{AgentName: agentName, Message: chat.Message{
		Role:             chat.MessageRoleAssistant,
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		Presentation:     parts,
	}}
}

// streamPlaceholderItem is the open row's stored body. Readers ignore it while stream_open=1.
func streamPlaceholderItem(agentName, messageID string) Item {
	return Item{Message: &Message{AgentName: agentName, Message: chat.Message{MessageID: messageID, Role: chat.MessageRoleAssistant}}}
}

// streamMessageID reads the message identity from a stored message body. It returns
// "" for an empty or unreadable body, because the identity is never guessed.
func streamMessageID(messageJSON string) string {
	var body struct {
		MessageID string `json:"message_id"`
	}
	if json.Unmarshal([]byte(messageJSON), &body) != nil {
		return ""
	}
	return body.MessageID
}

func validateMessageChunks(chunks []MessageChunk) error {
	for _, chunk := range chunks {
		switch chunk.Kind {
		case MessageChunkContent, MessageChunkReasoning:
		case MessageChunkTool:
			if chunk.Op == nil {
				return errors.New("session: tool stream chunk requires an operation")
			}
		default:
			return fmt.Errorf("session: unknown stream chunk kind %q", chunk.Kind)
		}
	}
	return nil
}

// streamStartHash fingerprints the placeholder and its initial chunks for receipt checks.
func streamStartHash(placeholder Item, chunks []MessageChunk) (string, error) {
	if chunks == nil {
		chunks = []MessageChunk{}
	}
	data, err := json.Marshal(struct {
		Placeholder Item
		Chunks      []MessageChunk
	}{placeholder, chunks})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// streamSeqCheck classifies a batch against the highest landed seq (-1 when none).
// It reports landed=true when the whole batch is already stored (ambiguous-commit retry).
func streamSeqCheck(maxSeq, firstSeq int64, count int) (bool, error) {
	last := firstSeq + int64(count) - 1
	switch {
	case maxSeq >= last:
		return true, nil
	case maxSeq == firstSeq-1:
		return false, nil
	default:
		return false, ErrWriteConflict
	}
}

// cloneStreamChunks deep-copies ops through JSON, as the SQL store does, so that stored
// chunks never alias caller-owned tool definitions.
func cloneStreamChunks(chunks []MessageChunk) ([]MessageChunk, error) {
	out := make([]MessageChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.Op != nil {
			data, err := json.Marshal(chunk.Op)
			if err != nil {
				return nil, fmt.Errorf("marshaling stream chunk: %w", err)
			}
			var op chat.AssistantPartOp
			if err := json.Unmarshal(data, &op); err != nil {
				return nil, fmt.Errorf("unmarshaling stream chunk: %w", err)
			}
			chunk.Op = &op
		}
		out = append(out, chunk)
	}
	return out, nil
}

func insertStreamChunksTx(ctx context.Context, tx *sql.Tx, itemID, firstSeq int64, chunks []MessageChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO session_message_chunks (item_id, seq, kind, text, op_json) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = stmt.Close() }()
	for i, chunk := range chunks {
		var op sql.NullString
		if chunk.Op != nil {
			data, err := json.Marshal(chunk.Op)
			if err != nil {
				return fmt.Errorf("marshaling stream chunk: %w", err)
			}
			op = sql.NullString{String: string(data), Valid: true}
		}
		if _, err := stmt.ExecContext(ctx, itemID, firstSeq+int64(i), string(chunk.Kind), chunk.Text, op); err != nil {
			return classifySQLiteContextError(ctx, err)
		}
	}
	return nil
}

// loadStreamChunks returns the chunks of one row in seq order.
func loadStreamChunks(ctx context.Context, q querier, itemID int64) ([]MessageChunk, error) {
	rows, err := q.QueryContext(ctx, `SELECT kind, text, op_json FROM session_message_chunks WHERE item_id = ? ORDER BY seq`, itemID)
	if err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()
	var chunks []MessageChunk
	for rows.Next() {
		var kind, text string
		var op sql.NullString
		if err := rows.Scan(&kind, &text, &op); err != nil {
			return nil, classifySQLiteContextError(ctx, err)
		}
		chunk := MessageChunk{Kind: MessageChunkKind(kind), Text: text}
		if op.Valid {
			var decoded chat.AssistantPartOp
			if err := json.Unmarshal([]byte(op.String), &decoded); err != nil {
				return nil, fmt.Errorf("unmarshaling stream chunk of item %d: %w", itemID, err)
			}
			chunk.Op = &decoded
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, classifySQLiteContextError(ctx, err)
	}
	return chunks, nil
}

// reconcileOpenRow resolves a row that a scan saw with stream_open=1. Its chunks and its
// current state are read by separate statements, so a settle or compaction may commit
// in between. The chunks of an open row only grow, and closing deletes them. So the
// row is re-read after the chunks: if it is still open, the chunks read are a valid
// earlier state. If it has closed, the canonical message_json is used, whatever the chunk
// read saw. It returns found=false when the row no longer exists. beforeChunks and
// afterChunks are test hooks and nil in production.
func (s *SQLiteSessionStore) reconcileOpenRow(ctx context.Context, q querier, row *sessionItemRow, beforeChunks, afterChunks func()) (bool, error) {
	if beforeChunks != nil {
		beforeChunks()
	}
	chunks, err := loadStreamChunks(ctx, q, row.id)
	if err != nil {
		return false, err
	}
	if afterChunks != nil {
		afterChunks()
	}
	var fresh sessionItemRow
	err = q.QueryRowContext(ctx,
		`SELECT stream_open, message_json, implicit, COALESCE(actor_pending, 0), COALESCE(actor_accepted, 0), COALESCE(actor_turn_id, ''), COALESCE(actor_input_mode, ''), input_origin, sender_id, sender_name, report_outcome
		 FROM session_items WHERE id = ? AND item_type = 'message'`, row.id).Scan(
		&fresh.streamOpen, &fresh.messageJSON, &fresh.implicit, &fresh.actorPending, &fresh.actorAccepted, &fresh.actorTurnID, &fresh.actorInputMode, &fresh.inputOrigin, &fresh.senderID, &fresh.senderName, &fresh.reportOutcome)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classifySQLiteContextError(ctx, err)
	}
	row.streamOpen = fresh.streamOpen
	row.messageJSON = fresh.messageJSON
	row.implicit, row.actorPending, row.actorAccepted = fresh.implicit, fresh.actorPending, fresh.actorAccepted
	row.actorTurnID, row.actorInputMode, row.inputOrigin = fresh.actorTurnID, fresh.actorInputMode, fresh.inputOrigin
	row.senderID, row.senderName, row.reportOutcome = fresh.senderID, fresh.senderName, fresh.reportOutcome
	if row.streamOpen {
		row.openBody = ReconstructStreamingMessage(row.agentName.String, chunks).Message
		row.openBody.MessageID = streamMessageID(row.messageJSON.String)
	}
	return true, nil
}

// AppendMessageChunks implements StreamingMessageStore for SQLite.
func (s *SQLiteSessionStore) AppendMessageChunks(ctx context.Context, sessionID string, start *StreamStart, itemID, firstSeq int64, chunks []MessageChunk) (int64, error) {
	if sessionID == "" {
		return 0, ErrEmptyID
	}
	if err := validateMessageChunks(chunks); err != nil {
		return 0, err
	}
	var id int64
	var err error
	if start != nil {
		id, err = s.startStreamingMessage(ctx, sessionID, *start, chunks)
	} else {
		if firstSeq < 0 {
			return 0, errors.New("session: stream chunk sequence must be non-negative")
		}
		id, err = s.appendStreamChunks(ctx, sessionID, itemID, firstSeq, chunks)
	}
	return id, classifySQLiteContextError(ctx, err)
}

func (s *SQLiteSessionStore) startStreamingMessage(ctx context.Context, sessionID string, start StreamStart, chunks []MessageChunk) (int64, error) {
	if start.WriteID == "" {
		return 0, ErrEmptyID
	}
	frozen, _, err := prepareAppend(streamPlaceholderItem(start.AgentName, start.MessageID))
	if err != nil {
		return 0, err
	}
	hash, err := streamStartHash(frozen, chunks)
	if err != nil {
		return 0, err
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	var originalHash string
	var withdrawn bool
	err = tx.QueryRowContext(ctx, `SELECT item_id, content_hash, withdrawn FROM session_append_receipts WHERE session_id = ? AND write_id = ?`, sessionID, start.WriteID).Scan(&id, &originalHash, &withdrawn)
	if err == nil {
		if originalHash != hash {
			return 0, ErrWriteConflict
		}
		if withdrawn {
			return 0, ErrInputWithdrawn
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	id, err = s.insertReceiptedItemTx(ctx, tx, sessionID, start.WriteID, frozen, hash)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_items SET stream_open = 1 WHERE id = ? AND session_id = ?`, id, sessionID); err != nil {
		return 0, err
	}
	if err := insertStreamChunksTx(ctx, tx, id, 0, chunks); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *SQLiteSessionStore) appendStreamChunks(ctx context.Context, sessionID string, itemID, firstSeq int64, chunks []MessageChunk) (int64, error) {
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var open bool
	err = tx.QueryRowContext(ctx, `SELECT stream_open FROM session_items WHERE session_id = ? AND id = ? AND item_type = 'message'`, sessionID, itemID).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !open {
		return 0, ErrStreamClosed
	}
	var maxSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) FROM session_message_chunks WHERE item_id = ?`, itemID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	landed, err := streamSeqCheck(maxSeq, firstSeq, len(chunks))
	if err != nil {
		return 0, err
	}
	if landed {
		return itemID, nil
	}
	if err := insertStreamChunksTx(ctx, tx, itemID, firstSeq, chunks); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return itemID, nil
}

// CloseStreamingMessage implements StreamingMessageStore for SQLite.
func (s *SQLiteSessionStore) CloseStreamingMessage(ctx context.Context, sessionID string, itemID int64, msg *Message) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := closeStreamTx(ctx, tx, sessionID, itemID, msg); err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	return classifySQLiteContextError(ctx, tx.Commit())
}

// CloseOpenStreams implements StreamingMessageStore for SQLite.
func (s *SQLiteSessionStore) CloseOpenStreams(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM session_items WHERE session_id = ? AND stream_open = 1 ORDER BY position`, sessionID)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	ids, err := scanOpenStreamIDs(rows)
	if err != nil {
		return classifySQLiteContextError(ctx, err)
	}
	for _, id := range ids {
		if err := closeStreamTx(ctx, tx, sessionID, id, nil); err != nil {
			return classifySQLiteContextError(ctx, err)
		}
	}
	return classifySQLiteContextError(ctx, tx.Commit())
}

// closeStreamTx settles one open row inside tx. It does not touch the transaction's commit.
func closeStreamTx(ctx context.Context, tx *sql.Tx, sessionID string, itemID int64, msg *Message) error {
	var agentName sql.NullString
	var open bool
	err := tx.QueryRowContext(ctx, `SELECT agent_name, stream_open FROM session_items WHERE session_id = ? AND id = ? AND item_type = 'message'`, sessionID, itemID).Scan(&agentName, &open)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !open {
		return ErrStreamClosed
	}
	var result sql.Result
	if msg != nil {
		msgJSON, err := json.Marshal(msg.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		result, err = tx.ExecContext(ctx,
			`UPDATE session_items SET message_json = ?, implicit = ?, actor_pending = ?, actor_accepted = ?, actor_turn_id = ?, actor_input_mode = ?, input_origin = ?, sender_id = ?, sender_name = ?, report_outcome = ?, stream_open = 0 WHERE session_id = ? AND id = ? AND stream_open = 1`,
			string(msgJSON), msg.Implicit, msg.Pending, msg.Accepted, msg.TurnID, msg.InputMode, msg.InputOrigin, msg.SenderID, msg.SenderName, msg.ReportOutcome, sessionID, itemID)
		if err != nil {
			return err
		}
	} else {
		// The canonical row keeps the identity of the open row it settles.
		var storedJSON string
		if err := tx.QueryRowContext(ctx, `SELECT message_json FROM session_items WHERE id = ?`, itemID).Scan(&storedJSON); err != nil {
			return err
		}
		storedMessageID := streamMessageID(storedJSON)
		chunks, err := loadStreamChunks(ctx, tx, itemID)
		if err != nil {
			return err
		}
		rebuilt := ReconstructStreamingMessage(agentName.String, chunks)
		rebuilt.Message.MessageID = storedMessageID
		msgJSON, err := json.Marshal(rebuilt.Message)
		if err != nil {
			return fmt.Errorf("marshaling message: %w", err)
		}
		result, err = tx.ExecContext(ctx,
			`UPDATE session_items SET message_json = ?, stream_open = 0 WHERE session_id = ? AND id = ? AND stream_open = 1`,
			string(msgJSON), sessionID, itemID)
		if err != nil {
			return err
		}
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrStreamClosed
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM session_message_chunks WHERE item_id = ?`, itemID)
	return err
}

// inMemoryStream is the chunk log of one open row, keyed by item ID in InMemorySessionStore.streams.
type inMemoryStream struct {
	sessionID string
	chunks    []MessageChunk
}

// AppendMessageChunks implements StreamingMessageStore for the in-memory store.
func (s *InMemorySessionStore) AppendMessageChunks(ctx context.Context, sessionID string, start *StreamStart, itemID, firstSeq int64, chunks []MessageChunk) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if sessionID == "" {
		return 0, ErrEmptyID
	}
	if err := validateMessageChunks(chunks); err != nil {
		return 0, err
	}
	stored, err := cloneStreamChunks(chunks)
	if err != nil {
		return 0, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if start != nil {
		return s.startStreamLocked(sessionID, *start, chunks, stored)
	}
	if firstSeq < 0 {
		return 0, errors.New("session: stream chunk sequence must be non-negative")
	}
	st, err := s.openStreamLocked(sessionID, itemID)
	if err != nil {
		return 0, err
	}
	landed, err := streamSeqCheck(int64(len(st.chunks))-1, firstSeq, len(stored))
	if err != nil {
		return 0, err
	}
	if !landed {
		st.chunks = append(st.chunks, stored...)
	}
	return itemID, nil
}

func (s *InMemorySessionStore) startStreamLocked(sessionID string, start StreamStart, chunks, stored []MessageChunk) (int64, error) {
	if start.WriteID == "" {
		return 0, ErrEmptyID
	}
	frozen, _, err := prepareAppend(streamPlaceholderItem(start.AgentName, start.MessageID))
	if err != nil {
		return 0, err
	}
	hash, err := streamStartHash(frozen, chunks)
	if err != nil {
		return 0, err
	}
	key := itemAppendKey{sessionID, start.WriteID}
	if receipt, ok := s.appendReceipts[key]; ok {
		if receipt.hash != hash {
			return 0, ErrWriteConflict
		}
		if receipt.withdrawn {
			return 0, ErrInputWithdrawn
		}
		return receipt.id, nil
	}
	session, ok := s.sessions.Load(sessionID)
	if !ok {
		return 0, ErrNotFound
	}
	id := s.messageID.Add(1)
	frozen.Message.ID = id
	session.mu.Lock()
	session.Messages = append(session.Messages, frozen)
	session.mu.Unlock()
	if s.appendReceipts == nil {
		s.appendReceipts = make(map[itemAppendKey]itemAppendReceipt)
	}
	s.appendReceipts[key] = itemAppendReceipt{id: id, hash: hash}
	if s.streams == nil {
		s.streams = make(map[int64]*inMemoryStream)
	}
	s.streams[id] = &inMemoryStream{sessionID: sessionID, chunks: stored}
	return id, nil
}

// openStreamLocked returns the open stream of itemID. A message that exists but has no
// open stream reports ErrStreamClosed. The caller must hold coordinationMu.
func (s *InMemorySessionStore) openStreamLocked(sessionID string, itemID int64) (*inMemoryStream, error) {
	session, ok := s.sessions.Load(sessionID)
	if !ok {
		return nil, ErrNotFound
	}
	if st := s.streams[itemID]; st != nil && st.sessionID == sessionID {
		return st, nil
	}
	session.mu.RLock()
	defer session.mu.RUnlock()
	for _, item := range session.Messages {
		if item.Message != nil && item.Message.ID == itemID {
			return nil, ErrStreamClosed
		}
	}
	return nil, ErrNotFound
}

// CloseStreamingMessage implements StreamingMessageStore for the in-memory store.
func (s *InMemorySessionStore) CloseStreamingMessage(ctx context.Context, sessionID string, itemID int64, msg *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" {
		return ErrEmptyID
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	st, err := s.openStreamLocked(sessionID, itemID)
	if err != nil {
		return err
	}
	session, _ := s.sessions.Load(sessionID)
	session.mu.Lock()
	defer session.mu.Unlock()
	s.settleStreamLocked(session, itemID, st, msg)
	return nil
}

// CloseOpenStreams implements StreamingMessageStore for the in-memory store.
func (s *InMemorySessionStore) CloseOpenStreams(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sessionID == "" {
		return ErrEmptyID
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	session, ok := s.sessions.Load(sessionID)
	if !ok {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for itemID, st := range s.streams {
		if st.sessionID == sessionID {
			s.settleStreamLocked(session, itemID, st, nil)
		}
	}
	return nil
}

// settleStreamLocked replaces the placeholder of itemID with msg, or with the reconstruction
// of st when msg is nil, and drops the stream. The caller must hold coordinationMu and session.mu.
func (s *InMemorySessionStore) settleStreamLocked(session *Session, itemID int64, st *inMemoryStream, msg *Message) {
	delete(s.streams, itemID)
	for i := range session.Messages {
		item := &session.Messages[i]
		if item.Message == nil || item.Message.ID != itemID {
			continue
		}
		var updated *Message
		if msg != nil {
			updated = cloneMessage(msg)
		} else {
			updated = cloneMessage(item.Message)
			updated.Message = cloneChatMessage(ReconstructStreamingMessage(item.Message.AgentName, st.chunks).Message)
			updated.Message.MessageID = item.Message.Message.MessageID
		}
		updated.ID = itemID
		session.Messages[i].Message = updated
		session.historyRevision++
		return
	}
}

// readStreamMessageLocked returns msg with the body reconstructed from its open stream, or msg
// itself when no stream of sessionID is open for it. The caller must hold coordinationMu.
func (s *InMemorySessionStore) readStreamMessageLocked(sessionID string, msg *Message) *Message {
	st := s.streams[msg.ID]
	if st == nil || st.sessionID != sessionID {
		return msg
	}
	out := cloneMessage(msg)
	out.Message = cloneChatMessage(ReconstructStreamingMessage(msg.AgentName, st.chunks).Message)
	out.Message.MessageID = msg.Message.MessageID
	return out
}

var (
	_ StreamingMessageStore = (*SQLiteSessionStore)(nil)
	_ StreamingMessageStore = (*InMemorySessionStore)(nil)
)

// scanOpenStreamIDs drains and closes rows before the caller issues writes on the same transaction.
func scanOpenStreamIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
