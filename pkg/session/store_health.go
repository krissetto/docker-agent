package session

import "context"

// StorePinger is an optional, bounded connectivity probe. It must not enumerate
// sessions, read transcripts, run migrations, or mutate persisted state. Store
// decorators can explicitly forward this capability without extending Store.
type StorePinger interface {
	Ping(context.Context) error
}

func (s *SQLiteSessionStore) Ping(ctx context.Context) error {
	var one int
	return classifySQLiteContextError(ctx, s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
}

func (*InMemorySessionStore) Ping(ctx context.Context) error {
	return ctx.Err()
}
