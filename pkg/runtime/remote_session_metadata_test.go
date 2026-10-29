package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/api"
)

// The remote projection keeps the top-level and capability live/historical flags
// together; neither is dropped or cross-copied.
func TestRemoteSessionMetadataProjectsLiveAndHistoricalFlags(t *testing.T) {
	for _, tc := range []struct {
		name               string
		historical         bool
		executionAvailable bool
	}{
		{"live", false, true},
		{"historical", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := remoteSessionMetadata(api.SessionMetadata{
				SessionID:          "session",
				AgentName:          "root",
				Historical:         tc.historical,
				ExecutionAvailable: tc.executionAvailable,
				Capabilities: api.SessionCapabilities{
					Historical:         tc.historical,
					ExecutionAvailable: tc.executionAvailable,
				},
			})
			got := meta.runtime()
			assert.Equal(t, tc.historical, got.Historical)
			assert.Equal(t, tc.executionAvailable, got.ExecutionAvailable)
			assert.Equal(t, tc.historical, got.Capabilities.Historical)
			assert.Equal(t, tc.executionAvailable, got.Capabilities.ExecutionAvailable)
		})
	}
}
