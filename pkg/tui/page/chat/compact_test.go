package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
)

func TestCompactSessionUsesSessionCapabilityWithoutMutatingRunState(t *testing.T) {
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, nil)
	p := newTestChatPage(t)
	p.app = a
	p.working = true
	p.lifecycle.Streams = []lifecycle.Stream{{}, {}}
	p.messageQueue = []queuedMessage{{content: "later"}}

	cmd := p.CompactSession("focus")
	require.NotNil(t, cmd)
	assert.Nil(t, cmd())
	assert.Equal(t, []string{"focus"}, handle.compactPrompts)
	assert.True(t, p.working)
	assert.Equal(t, 2, p.lifecycle.Depth())
	require.Len(t, p.messageQueue, 1)
}

func TestCompactSessionUnsupportedSessionIsVisible(t *testing.T) {
	p := newTestChatPage(t)
	sess := session.New()
	p.app = app.New(t.Context(), nil, sess, runtime.SessionBinding{})

	cmd := p.CompactSession("")
	require.NotNil(t, cmd)
	note, ok := cmd().(notification.ShowMsg)
	require.True(t, ok)
	assert.Equal(t, notification.TypeInfo, note.Type)
	assert.Contains(t, note.Text, "unavailable")
}
