package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestEnterQueuesWhenBusySendModeCoherent(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": false, "steer": false, "queue": true, "bogus": false} {
		assert.Equal(t, want, enterQueuesWhenBusy(&userconfig.Settings{BusySendMode: raw}), "busy_send_mode %q", raw)
	}
	assert.False(t, enterQueuesWhenBusy(nil))
}

func TestLeanSendKeysFollowBusyStateAndSendMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		seq       string
		busy      bool
		queueMode bool
		wantSent  bool // steered into the active turn
		wantQueue bool // end-of-turn follow-up
	}{
		{name: "idle Enter sends normally", seq: "\r"},
		{name: "idle Alt+Enter sends normally", seq: "\x1b\r"},
		{name: "busy Enter steers by default", seq: "\r", busy: true, wantSent: true},
		{name: "busy Enter queues with busy_send_mode queue", seq: "\r", busy: true, queueMode: true, wantQueue: true},
		{name: "busy Alt+Enter queues by default", seq: "\x1b\r", busy: true, wantQueue: true},
		{name: "busy Alt+Enter queues with busy_send_mode queue", seq: "\x1b\r", busy: true, queueMode: true, wantQueue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, handle := sessionModel(t)
			m.busyEnterQueue = tc.queueMode
			if tc.busy {
				m.lifecycle.Status = runtime.SessionStateRunning
			} else {
				m.lifecycle.Status = runtime.SessionStateSettled
			}
			submitted := len(handle.submitted)
			m.screen.Editor.SetText("message")
			m.handleKey(t.Context(), leanHelpKey(t, tc.seq))
			steers := 0
			for _, pending := range m.pendingUsers {
				if pending.Kind == ui.PendingUserSteer {
					steers++
				}
			}
			assert.Equal(t, boolCount(tc.wantSent), steers, "steer entries only for steered sends")
			switch {
			case tc.wantSent:
				require.Len(t, handle.sent, 1)
				assert.Equal(t, ui.PendingUserSteer, m.pendingUsers[0].Kind)
				assert.Len(t, handle.submitted, submitted)
			case tc.wantQueue:
				require.NotEmpty(t, m.queue)
				assert.Equal(t, ui.PendingUserFollowUp, m.queue[0].Kind)
				assert.Empty(t, handle.sent)
				assert.Len(t, handle.submitted, submitted)
			default:
				assert.Empty(t, handle.sent, "idle sends are not steers")
				assert.Len(t, handle.submitted, submitted+1, "idle sends start a normal turn")
			}
			assert.True(t, m.screen.Editor.IsEmpty())
		})
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}
