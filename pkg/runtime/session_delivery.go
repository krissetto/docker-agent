package runtime

import "context"

// Detached notes are addressed only to known session drivers. Idle sessions
// retain runtime-authored notes for their next execution; absent inboxes reject
// delivery rather than acknowledging volatile future-session buffering.

func (r *LocalRuntime) deliverOrBuffer(ctx context.Context, sessionID, content string) {
	r.sessionDrivers.PostReliable(ctx, sessionID, QueuedMessage{Content: content})
}

func (r *LocalRuntime) drainSessionSteer(sessionID string) []QueuedMessage {
	if d, ok := r.sessionDrivers.Lookup(sessionID); ok {
		return d.drainBoundarySteering()
	}
	return nil
}
