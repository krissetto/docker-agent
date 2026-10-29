package runtime

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// post reports the admission decision: a turn that waits behind busy or idle
// work is queued, while steering delivered at the next boundary is not. Input
// with a request ID reports the same decision, and rejected input never reports
// queued.
func TestPostQueuedAdmissionAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requestID string
		mode      string
		running   bool
		rejected  bool
		want      bool
	}{
		{"empty id turn while idle is queued", "", "turn", false, false, true},
		{"empty id turn behind running work is queued", "", "turn", true, false, true},
		{"empty id steer while running is immediate", "", "steer", true, false, false},
		{"request id turn behind running work is queued", "req-turn", "turn", true, false, true},
		{"request id steer while running is immediate", "req-steer", "steer", true, false, false},
		{"empty id rejected while stopped", "", "turn", false, true, false},
		{"request id rejected while stopped", "req-rejected", "turn", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("done"), coordinationReply("done"))
				parent := coordinationCreate(t, owner.Runtime(), "parent", "")
				d := parent.(*sessionHandle).driver
				if tc.running {
					require.NoError(t, d.ownerCall(t.Context(), func() error {
						d.phase = sessionRunning
						d.activeRequestID = "active"
						return nil
					}))
				}
				if tc.rejected {
					require.NoError(t, d.ownerCall(t.Context(), func() error {
						d.stopped = true
						return nil
					}))
				}
				msg := QueuedMessage{Content: "input", RequestID: tc.requestID, InputOrigin: session.InputOriginRuntime, InputMode: tc.mode}
				queued, err := d.post(t.Context(), msg, !tc.running)
				if tc.rejected {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, tc.want, queued)
			})
		})
	}
}
