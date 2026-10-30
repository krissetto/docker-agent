package chat

import "context"

// inputScope identifies one chat page lifetime, including a reload of the same
// conversation. Results of asynchronous input submissions carry the scope that
// dispatched them, and the page accepts a result only while that scope is live.
//
// Closing a scope never withdraws a submission the runtime already accepted:
// accepted messages belong to the session and are not cancelled on close or
// reload. Only the UI result is dropped.
type inputScope struct {
	ctx    context.Context //nolint:containedctx // Owns the lifetime of in-flight page submissions.
	cancel context.CancelFunc
}

func newInputScope(ctx context.Context) *inputScope {
	ctx, cancel := context.WithCancel(ctx)
	return &inputScope{ctx: ctx, cancel: cancel}
}

// live reports whether the owning page lifetime is still open.
func (s *inputScope) live() bool { return s.ctx.Err() == nil }

// close stops submissions that have not reached the runtime yet and drops the
// UI results of those that have. It never withdraws an accepted message.
func (s *inputScope) close() { s.cancel() }

// inputResult tags an asynchronous submission result with the scope that
// dispatched it.
type inputResult struct{ scope *inputScope }

func (r inputResult) inputOrigin() *inputScope { return r.scope }

// ownsInputScope reports whether a result dispatched under scope may still be applied to this page.
func (p *chatPage) ownsInputScope(scope *inputScope) bool {
	return scope != nil && scope == p.inputScope && scope.live()
}
