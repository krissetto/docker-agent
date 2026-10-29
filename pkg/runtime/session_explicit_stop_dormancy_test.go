package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An explicit user stop still notifies observers of the dormancy change, unlike
// internal retirement, which is silent.
func TestExplicitStopEmitsDormancyNotification(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 32})
	require.NoError(t, err)
	defer observation.Cancel()

	controller, ok := handle.(SessionTreeController)
	require.True(t, ok, "local handles expose subtree stop")
	require.NoError(t, controller.StopSubtree(t.Context()))

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case envelope := <-observation.Events:
			if event, ok := envelope.Event.(*DormancyChangedEvent); ok {
				assert.True(t, event.Stopped)
				return
			}
		case <-deadline.C:
			require.FailNow(t, "explicit stop did not publish a dormancy notification")
		}
	}
}
