package runtime

import "context"

// SessionViewInfoReader confirms persisted identity and view policy without
// reserving, publishing, restoring, or starting canonical execution.
// Runtime decorators must forward it with the same owner lifetime as preparation.
type SessionViewInfoReader interface {
	ConfirmedSessionViewInfo(ctx context.Context, sessionID string) (PreparedSessionViewInfo, error)
}

var (
	_ SessionViewInfoReader = (*localSessionRuntimeView)(nil)
	_ SessionViewPreparer   = (*localSessionRuntimeView)(nil)
)
