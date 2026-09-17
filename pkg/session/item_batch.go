package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// ItemBatchAppender atomically appends an immutable input batch. Receipts retain
// request identity after messages have been promoted, edited or removed.
type ItemBatchAppender interface {
	AppendItems(ctx context.Context, sessionID, requestID string, items []Item) (ItemBatchReceipt, error)
	LookupItemBatch(ctx context.Context, sessionID, requestID string, items []Item) (ItemBatchReceipt, bool, error)
}
type ItemBatchModelAppender interface {
	AppendItemsWithModel(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef string) (ItemBatchReceipt, error)
}

type ItemBatchBindingAppender interface {
	AppendItemsWithBinding(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef, activeAgent string) (ItemBatchReceipt, error)
}

type ItemBatchReceipt struct {
	IDs       []int64
	Duplicate bool
}
type itemBatchReceipt struct {
	IDs  []int64
	Hash string
}

func prepareItemBatch(items []Item) ([]Item, string, error) {
	frozen := make([]Item, len(items))
	for i, item := range items {
		var err error
		frozen[i], _, err = prepareAppend(item)
		if err != nil {
			return nil, "", err
		}
	}
	fingerprint := any(frozen)
	// A legacy run receipt fingerprints the original request, not evaluator
	// output or the active agent selected by that request. Retrying after a
	// switch must not reinterpret commands against the new active agent.
	if len(frozen) > 0 {
		marker := frozen[len(frozen)-1].Message
		if marker != nil && marker.InputMode == "legacy_run" {
			fingerprint = struct{ Request, Model string }{marker.Message.ReasoningContent, marker.Message.Model}
		}
	}
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(data)
	return frozen, hex.EncodeToString(hash[:]), nil
}

func (s *InMemorySessionStore) AppendItems(ctx context.Context, sessionID, requestID string, items []Item) (ItemBatchReceipt, error) {
	return s.AppendItemsWithModel(ctx, sessionID, requestID, items, "", "")
}

func (s *InMemorySessionStore) AppendItemsWithModel(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef string) (ItemBatchReceipt, error) {
	return s.AppendItemsWithBinding(ctx, sessionID, requestID, items, agentName, modelRef, "")
}

func (s *InMemorySessionStore) AppendItemsWithBinding(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef, activeAgent string) (receipt ItemBatchReceipt, err error) {
	if err := ctx.Err(); err != nil {
		return ItemBatchReceipt{}, err
	}
	if sessionID == "" || requestID == "" {
		return ItemBatchReceipt{}, ErrEmptyID
	}
	frozen, hash, err := prepareItemBatch(items)
	if err != nil {
		return ItemBatchReceipt{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	key := itemAppendKey{sessionID, requestID}
	if receipt, ok := s.batchReceipts[key]; ok {
		if receipt.Hash != hash {
			return ItemBatchReceipt{}, ErrWriteConflict
		}
		return ItemBatchReceipt{IDs: append([]int64(nil), receipt.IDs...), Duplicate: true}, nil
	}
	stored, ok := s.sessions.Load(sessionID)
	if !ok {
		return ItemBatchReceipt{}, ErrNotFound
	}
	ids := make([]int64, len(frozen))
	for i := range frozen {
		ids[i] = s.messageID.Add(1)
		if frozen[i].Message != nil {
			frozen[i].Message.ID = ids[i]
		}
	}
	if err := ctx.Err(); err != nil {
		return ItemBatchReceipt{}, err
	}
	if modelRef != "" {
		stored.SetAgentModelOverride(agentName, modelRef)
	}
	stored.mu.Lock()
	if activeAgent != "" {
		stored.AgentName = activeAgent
	}
	stored.Messages = append(stored.Messages, frozen...)
	stored.mu.Unlock()
	if s.batchReceipts == nil {
		s.batchReceipts = make(map[itemAppendKey]itemBatchReceipt)
	}
	s.batchReceipts[key] = itemBatchReceipt{IDs: ids, Hash: hash}
	return ItemBatchReceipt{IDs: append([]int64(nil), ids...)}, nil
}

func (s *SQLiteSessionStore) AppendItems(ctx context.Context, sessionID, requestID string, items []Item) (ItemBatchReceipt, error) {
	return s.AppendItemsWithModel(ctx, sessionID, requestID, items, "", "")
}

func (s *SQLiteSessionStore) AppendItemsWithModel(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef string) (ItemBatchReceipt, error) {
	return s.AppendItemsWithBinding(ctx, sessionID, requestID, items, agentName, modelRef, "")
}

func (s *SQLiteSessionStore) AppendItemsWithBinding(ctx context.Context, sessionID, requestID string, items []Item, agentName, modelRef, activeAgent string) (receipt ItemBatchReceipt, err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	if sessionID == "" || requestID == "" {
		return receipt, ErrEmptyID
	}
	frozen, hash, err := prepareItemBatch(items)
	if err != nil {
		return receipt, err
	}
	tx, err := beginSQLiteWrite(ctx, s.db)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldHash, rawIDs string
	err = tx.QueryRowContext(ctx, `SELECT content_hash,item_ids FROM session_input_batches WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&oldHash, &rawIDs)
	if err == nil {
		if oldHash != hash {
			return receipt, ErrWriteConflict
		}
		err = json.Unmarshal([]byte(rawIDs), &receipt.IDs)
		receipt.Duplicate = true
		return receipt, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return receipt, err
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id=?`, sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return receipt, ErrNotFound
	} else if err != nil {
		return receipt, err
	}
	if modelRef != "" || activeAgent != "" {
		snapshot, loadErr := scanSession(tx.QueryRowContext(ctx, "SELECT "+sessionSelectColumns+" FROM sessions WHERE id=?", sessionID))
		if loadErr != nil {
			return receipt, loadErr
		}
		if modelRef != "" {
			snapshot.SetAgentModelOverride(agentName, modelRef)
		}
		if activeAgent != "" {
			snapshot.AgentName = activeAgent
		}
		if err := s.upsertSessionRowTx(ctx, tx, snapshot); err != nil {
			return receipt, err
		}
	}
	var position int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),-1)+1 FROM session_items WHERE session_id=?`, sessionID).Scan(&position); err != nil {
		return receipt, err
	}
	for i, item := range frozen {
		if err := s.addItemTx(ctx, tx, sessionID, position+i, item); err != nil {
			return receipt, err
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&id); err != nil {
			return receipt, err
		}
		receipt.IDs = append(receipt.IDs, id)
	}
	ids, err := json.Marshal(receipt.IDs)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_input_batches(session_id,request_id,content_hash,item_ids) VALUES(?,?,?,?)`, sessionID, requestID, hash, string(ids)); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

func (s *InMemorySessionStore) LookupItemBatch(ctx context.Context, sessionID, requestID string, items []Item) (ItemBatchReceipt, bool, error) {
	if err := ctx.Err(); err != nil {
		return ItemBatchReceipt{}, false, err
	}
	_, hash, err := prepareItemBatch(items)
	if err != nil {
		return ItemBatchReceipt{}, false, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	receipt, ok := s.batchReceipts[itemAppendKey{sessionID, requestID}]
	if !ok {
		return ItemBatchReceipt{}, false, nil
	}
	if receipt.Hash != hash {
		return ItemBatchReceipt{}, true, ErrWriteConflict
	}
	return ItemBatchReceipt{IDs: append([]int64(nil), receipt.IDs...), Duplicate: true}, true, nil
}

func (s *SQLiteSessionStore) LookupItemBatch(ctx context.Context, sessionID, requestID string, items []Item) (receipt ItemBatchReceipt, found bool, err error) {
	defer func() { err = classifySQLiteContextError(ctx, err) }()
	_, hash, err := prepareItemBatch(items)
	if err != nil {
		return receipt, false, err
	}
	var oldHash, rawIDs string
	err = s.db.QueryRowContext(ctx, `SELECT content_hash,item_ids FROM session_input_batches WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&oldHash, &rawIDs)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if oldHash != hash {
		return receipt, true, ErrWriteConflict
	}
	receipt.Duplicate = true
	err = json.Unmarshal([]byte(rawIDs), &receipt.IDs)
	return receipt, true, err
}
