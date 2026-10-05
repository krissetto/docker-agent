package chat

import (
	"testing"

	"github.com/stretchr/testify/require"

	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

func TestWorkingNotificationsCanArriveInReverseOrder(t *testing.T) {
	page := &chatPage{}
	start := page.setWorking(true)
	stop := page.setWorking(false)
	require.False(t, page.IsWorking())
	require.False(t, stop().(msgtypes.WorkingStateChangedMsg).Working)
	require.True(t, start().(msgtypes.WorkingStateChangedMsg).Working, "queued commands capture stale state even after stop")
}
