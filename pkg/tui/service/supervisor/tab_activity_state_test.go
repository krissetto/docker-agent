package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// A queued session is admitted but not begun, so its tab shows pending rather
// than running; the running state then replaces it.
func TestTabActivityQueuedIsPendingUntilRunning(t *testing.T) {
	s := newTestSupervisor([]string{"tab"}, "other")

	queued := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateQueued}}
	s.applyPresentation("tab", nil, 0, queued, nil)
	tabs, _ := s.GetTabs()
	require.Len(t, tabs, 1)
	assert.Equal(t, messages.TabActivityPending, tabs[0].Activity)

	running := &app.PresentationState{Status: runtime.SessionStatus{SessionID: "session", State: runtime.SessionStateRunning}}
	s.applyPresentation("tab", nil, 0, running, nil)
	tabs, _ = s.GetTabs()
	require.Len(t, tabs, 1)
	assert.Equal(t, messages.TabActivityRunning, tabs[0].Activity)
}
