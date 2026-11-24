package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/docker/docker-agent/pkg/chat"
)

// ViewHistoryStore can provide a transcript projection without hydrating an owner.
// Positions exposed to callers are dense, while SQLite's ordering keys may have holes.
type ViewHistoryStore interface {
	ReadViewHistoryState(ctx context.Context, id string) (int, []BoundedItem, error)
	ReadViewHistoryPage(ctx context.Context, id string, total, before, maxItems, maxItemBytes int, storageBefore int64, fits func(BoundedItemPage) bool) (BoundedItemPage, error)
}

const transcriptItemFilter = `(item_type != 'subsession' OR subsession_id IS NOT NULL)`

func (s *SQLiteSessionStore) ReadViewHistoryState(ctx context.Context, id string) (int, []BoundedItem, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, nil, classifySQLiteContextError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	total, err := viewHistoryCount(ctx, tx, id)
	if err != nil {
		return 0, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+sessionItemColumns+` FROM session_items WHERE session_id = ? AND actor_pending = 1 AND actor_accepted = 1 ORDER BY position`, id)
	if err != nil {
		return 0, nil, classifySQLiteContextError(ctx, err)
	}
	raw, err := scanSessionItemRows(ctx, rows)
	if err != nil {
		return 0, nil, err
	}
	var pending []BoundedItem
	for _, row := range raw {
		item, err := s.decodeSessionItem(ctx, tx, id, row, false)
		if err != nil {
			return 0, nil, err
		}
		var later int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_items WHERE session_id = ? AND position > ? AND `+transcriptItemFilter, id, row.position).Scan(&later); err != nil {
			return 0, nil, classifySQLiteContextError(ctx, err)
		}
		pending = append(pending, BoundedItem{Position: total - later - 1, Item: item})
	}
	return total, pending, nil
}

func viewHistoryCount(ctx context.Context, q querier, id string) (int, error) {
	var total int
	if err := q.QueryRowContext(ctx, `SELECT transcript_item_count FROM sessions WHERE id = ?`, id).Scan(&total); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, classifySQLiteContextError(ctx, err)
	}
	return total, nil
}

func (s *SQLiteSessionStore) ReadViewHistoryPage(ctx context.Context, id string, total, before, maxItems, maxItemBytes int, storageBefore int64, fits func(BoundedItemPage) bool) (BoundedItemPage, error) {
	if id == "" || before < 0 || before > total || maxItems < 1 || maxItemBytes < 1 || fits == nil || (storageBefore == 0 && before != total) {
		return BoundedItemPage{}, errors.New("invalid stored history page request")
	}
	if storageBefore == 0 {
		storageBefore = math.MaxInt64
	}
	// Fetch only the selected descending window. Avoid transferring large blobs
	// or replaying an unbounded chunk stream just to mark an oversized item omitted.
	const columns = `id, position, item_type, agent_name,
		CASE WHEN length(CAST(message_json AS BLOB)) <= :maxBytes THEN message_json END,
		implicit, actor_pending, actor_accepted, actor_turn_id, actor_input_mode, input_origin, sender_id, sender_name, report_outcome, subsession_id,
		CASE WHEN length(CAST(summary_text AS BLOB)) <= :maxBytes THEN summary_text END,
		COALESCE(first_kept_entry, 0), cost, COALESCE(model, ''),
		CASE WHEN length(CAST(usage_json AS BLOB)) <= :maxBytes THEN usage_json ELSE '' END,
		stream_open,
		COALESCE(length(CAST(message_json AS BLOB)), 0) + COALESCE(length(CAST(summary_text AS BLOB)), 0) + COALESCE(length(CAST(usage_json AS BLOB)), 0) > :maxBytes`
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM session_items WHERE session_id = :id AND position < :before AND `+transcriptItemFilter+` ORDER BY position DESC LIMIT :count`,
		sql.Named("maxBytes", maxItemBytes), sql.Named("id", id), sql.Named("before", storageBefore), sql.Named("count", min(before, maxItems)))
	if err != nil {
		return BoundedItemPage{}, classifySQLiteContextError(ctx, err)
	}
	raw, err := scanSessionItemRowsMode(ctx, rows, true)
	if err != nil {
		return BoundedItemPage{}, err
	}
	if len(raw) != min(before, maxItems) {
		return BoundedItemPage{}, fmt.Errorf("stored history window changed: %w", ErrNotFound)
	}
	return readBoundedItemWindow(before, total, maxItems, maxItemBytes, fits, func(position int) (Item, int64, bool, error) {
		row := raw[before-position-1]
		key := int64(row.position)
		if row.oversized {
			return Item{}, key, true, nil
		}
		if row.streamOpen {
			item, oversized, err := s.readOpenHistoryItem(ctx, row, maxItemBytes)
			return item, key, oversized, err
		}
		item, err := s.decodeSessionItem(ctx, s.db, id, row, false)
		return item, key, false, err
	}, nil, nil)
}

// Open rows can survive a crash. Reconstruct only the bounded prefix that could
// fit this item; observing a live owner uses the in-memory presentation instead.
func (s *SQLiteSessionStore) readOpenHistoryItem(ctx context.Context, item sessionItemRow, maxBytes int) (Item, bool, error) {
	const maxChunks = 1024
	rows, err := s.db.QueryContext(ctx, `SELECT kind,
		CASE WHEN length(CAST(text AS BLOB)) <= ? THEN text END,
		CASE WHEN length(CAST(op_json AS BLOB)) <= ? THEN op_json END,
		(length(CAST(text AS BLOB)) + COALESCE(length(CAST(op_json AS BLOB)), 0)) > ?
		FROM session_message_chunks WHERE item_id = ? ORDER BY seq LIMIT ?`, maxBytes, maxBytes, maxBytes, item.id, maxChunks+1)
	if err != nil {
		return Item{}, false, classifySQLiteContextError(ctx, err)
	}
	defer rows.Close()
	var chunks []MessageChunk
	bytes := 0
	for rows.Next() {
		var kind MessageChunkKind
		var text, op sql.NullString
		var oversized bool
		if err := rows.Scan(&kind, &text, &op, &oversized); err != nil {
			return Item{}, false, classifySQLiteContextError(ctx, err)
		}
		bytes += len(text.String) + len(op.String)
		if oversized || bytes > maxBytes || len(chunks) >= maxChunks {
			return Item{}, true, nil
		}
		chunk := MessageChunk{Kind: kind, Text: text.String}
		if op.Valid {
			var decoded chat.AssistantPartOp
			if err := json.Unmarshal([]byte(op.String), &decoded); err != nil {
				return Item{}, false, err
			}
			chunk.Op = &decoded
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return Item{}, false, classifySQLiteContextError(ctx, err)
	}
	msg := ReconstructStreamingMessage(item.agentName.String, chunks)
	msg.ID, msg.Implicit = item.id, item.implicit
	msg.Message.MessageID = streamMessageID(item.messageJSON.String)
	return Item{Message: &msg}, false, nil
}
