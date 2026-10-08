package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestSteeringCanonicalTransitionsSurviveCanceledEnvelope(t *testing.T) {
	for _, kind := range []string{"accepted", "edited", "promoted", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			a := &App{cancelledRequests: map[string]struct{}{"active": {}}}
			var event runtime.Event
			switch kind {
			case "accepted":
				event = runtime.PendingUserMessageAccepted("s", "incoming", "body", nil, 0)
			case "edited":
				event = runtime.PendingUserMessageEdited("s", "incoming", "edited body", nil, 0)
			case "promoted":
				event = runtime.PendingUserMessagePromoted("s", "incoming", "body", nil, 0)
			case "canceled":
				event = runtime.PendingUserMessageCanceled("s", "incoming", 0)
			}
			require.Same(t, event, a.filterBridgedEvent("incoming", event), "different communication identity survives active cancellation")
			require.Same(t, event, a.filterBridgedEvent("active", event), "matching envelope must preserve canonical input transition")
			require.Nil(t, a.filterBridgedEvent("active", &runtime.AgentChoiceEvent{Content: "stale"}), "stale canceled output remains muted")
		})
	}
}
