package runtime

import "context"

func subscribeSessionEventsForTest(r *LocalRuntime, sessionID string) ([]Event, <-chan Event, func()) {
	if driver, ok := r.sessionDrivers.Lookup(sessionID); ok {
		return driver.Subscribe(defaultEventChannelCapacity)
	}
	return r.sessionEvents.Subscribe(sessionID, defaultEventChannelCapacity)
}

func deliverMessageForTest(r *LocalRuntime, ctx context.Context, sessionID, content string) bool {
	handle, err := r.SessionByID(sessionID)
	if err != nil {
		return false
	}
	_, err = handle.Submit(ctx, TurnInput{Content: content})
	return err == nil
}
