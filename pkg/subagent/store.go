package subagent

import (
	"context"
	"sync"
)

// Store persists swarm snapshots keyed by their owning (top-level) session.
// It is deliberately separate from the session store interface: subagent data
// lives in its own table/database. The runtime auto-detects this interface on
// the configured session store (the built-in SQLite store implements it with a
// dedicated table), so embedders with their own database (e.g. Postgres) can
// either implement these two methods on their session store or supply a
// dedicated implementation via runtime.WithSubagentStore.
//
// Implementations must be safe for concurrent use.
type Store interface {
	// SaveTree upserts the snapshot for a session.
	SaveTree(ctx context.Context, sessionID string, snapshot Snapshot) error
	// LoadTree returns the stored snapshot, or nil (no error) when the
	// session has none.
	LoadTree(ctx context.Context, sessionID string) (*Snapshot, error)
}

type DurableStore interface {
	Store
	Durability() Durability
}

// InMemoryStore is the fallback Store used when the configured session store
// provides no persistent backend.
type InMemoryStore struct {
	mu    sync.RWMutex
	trees map[string]Snapshot
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{trees: map[string]Snapshot{}}
}

func (s *InMemoryStore) Durability() Durability { return DurabilityVolatile }

func (s *InMemoryStore) SaveTree(_ context.Context, sessionID string, snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trees[sessionID] = snapshot
	return nil
}

func (s *InMemoryStore) LoadTree(_ context.Context, sessionID string) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.trees[sessionID]
	if !ok {
		return nil, nil
	}
	return &snapshot, nil
}
