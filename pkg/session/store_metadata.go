package session

import (
	"context"
	"database/sql"
	"errors"
	"maps"
)

// MetadataStore reads persisted identity without transcript hydration.
// Stores without this capability retain the ordinary recursive read contract.
type MetadataStore interface {
	GetSessionMetadata(context.Context, string) (*Session, error)
}

func ReadSessionMetadata(ctx context.Context, store Store, id string) (*Session, error) {
	if metadata, ok := store.(MetadataStore); ok {
		return metadata.GetSessionMetadata(ctx, id)
	}
	return store.GetSession(ctx, id)
}

// MetadataSnapshot omits transcript, topology and history-sized bookkeeping.
func (s *Session) MetadataSnapshot() *Session {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &Session{ID: s.ID, ParentID: s.ParentID, AgentName: s.AgentName, AgentID: s.AgentID, WorkingDir: s.WorkingDir,
		Attributes: maps.Clone(s.Attributes), AgentModelOverrides: maps.Clone(s.AgentModelOverrides), AsyncSubagent: s.AsyncSubagent}
}

func (s *InMemorySessionStore) GetSessionMetadata(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	if id == "" {
		return nil, ErrEmptyID
	}
	stored, ok := s.sessions.Load(id)
	if !ok {
		return nil, ErrNotFound
	}
	return stored.MetadataSnapshot(), nil
}

func (s *SQLiteSessionStore) GetSessionMetadata(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrEmptyID
	}
	row := s.db.QueryRowContext(ctx, "SELECT "+sessionSelectColumns+" FROM sessions WHERE id = ?", id)
	metadata, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return metadata, classifySQLiteContextError(ctx, err)
}

// ChildRecordReader checks one persisted membership without loading siblings.
type ChildRecordReader interface {
	LoadChild(context.Context, string) (ChildRecord, error)
}

func (s *InMemorySessionStore) LoadChild(ctx context.Context, id string) (ChildRecord, error) {
	if err := ctx.Err(); err != nil {
		return ChildRecord{}, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	record, ok := s.children[id]
	if !ok {
		return ChildRecord{}, ErrNotFound
	}
	return record, nil
}

func (s *SQLiteSessionStore) LoadChild(ctx context.Context, id string) (ChildRecord, error) {
	record, err := loadChildRecord(ctx, s.db, id)
	return record, classifySQLiteContextError(ctx, err)
}

// OwnHistoryStore reads a session's own transcript while retaining only
// ID-only child references. It never recursively hydrates child transcripts.
type OwnHistoryStore interface {
	GetSessionOwnHistory(context.Context, string) (*Session, error)
}

// ReadSessionOwnHistory requires a store that can avoid recursively reading
// embedded child sessions. It deliberately has no unsafe GetSession fallback.
func ReadSessionOwnHistory(ctx context.Context, store Store, id string) (*Session, error) {
	reader, ok := store.(OwnHistoryStore)
	if !ok {
		return nil, errors.New("session store cannot read selected history without child transcripts")
	}
	return reader.GetSessionOwnHistory(ctx, id)
}

func (s *InMemorySessionStore) GetSessionOwnHistory(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.coordinationMu.Lock()
	defer s.coordinationMu.Unlock()
	stored, ok := s.sessions.Load(id)
	if !ok {
		return nil, ErrNotFound
	}
	return stored.OwnSnapshot(), nil
}

func (s *SQLiteSessionStore) GetSessionOwnHistory(ctx context.Context, id string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrEmptyID
	}
	return s.loadSessionWithItems(ctx, s.db, id, false)
}
