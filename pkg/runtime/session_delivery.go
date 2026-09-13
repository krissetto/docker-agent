package runtime

import "context"

// Detached message delivery is owned by per-session drivers: a live session
// buffers input for its next steering drain; an idle known session can be
// woken by runtime-authored notes; unknown sessions keep wakeable notes until
// their session object is seen.

func (r *LocalRuntime) deliverOrBuffer(ctx context.Context, sessionID, content string) {
	r.sessionDrivers.PostReliable(ctx, sessionID, QueuedMessage{Content: content})
}

func (r *LocalRuntime) drainSessionSteer(sessionID string) []QueuedMessage {
	if d, ok := r.sessionDrivers.Lookup(sessionID); ok {
		return d.drainBoundarySteering()
	}
	return nil
}
