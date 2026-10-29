package chat

import (
	"testing"

	"github.com/stretchr/testify/require"

	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

// The wrapper delegates to messages' concrete getter and reports false when no
// handoff is in flight; the active phase is covered by the bounded residual test.
func TestChatPageReplyHandoffGetterIsIdleWithoutStream(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	require.False(t, p.ReplyHandoffActive(), "an idle page has no initial reply handoff")
}
