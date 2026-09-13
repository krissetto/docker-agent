package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/session"
)

func TestInputVisibilityUsesOnlyTypedOrigin(t *testing.T) {
	for _, origin := range []session.InputOrigin{"", "future", session.InputOriginUser, session.InputOriginAgent, session.InputOriginRuntime} {
		msg := session.UserMessage("<system_info>literal</system_info>")
		msg.InputOrigin = origin
		assert.Equal(t, origin != session.InputOriginAgent && origin != session.InputOriginRuntime, IsUserInput(origin))
		assert.True(t, VisibleTranscriptMessage(msg))
	}
}

func TestInputReplayUsesIdentityAndPendingSnapshotBoundary(t *testing.T) {
	sess := session.New()
	shown := session.UserMessage("same")
	shown.TurnID = "shown"
	sess.AddMessage(shown)
	pending := session.UserMessage("same")
	pending.TurnID, pending.Pending = "pending", true
	sess.AddMessage(pending)
	var replay InputReplay
	replay.Reset(sess)
	assert.False(t, replay.Consume("shown", 0))
	assert.True(t, replay.Consume("pending", 1))
	assert.False(t, replay.Consume("pending", 1))
	assert.True(t, replay.Consume("different", 2))
	assert.False(t, replay.Consume("", 2))
	assert.True(t, replay.Consume("", -1))
	assert.True(t, replay.Consume("", -1), "unknown identity is not content-deduplicated")
}

func TestRuntimeNoticeUsesTypedAttributionOnly(t *testing.T) {
	assert.Equal(t, "Runtime update received", RuntimeInputNotice("", ""))
	assert.Equal(t, "subagent (12345) has replied", RuntimeInputNotice("", "12345678-long-id"))
	assert.Equal(t, "worker (12345) has replied", RuntimeInputNotice("worker", "12345678-long-id"))
	assert.Equal(t, "worker has replied", RuntimeInputNotice("worker", ""))
	assert.Equal(t, "worker (short)", InputSender("worker", "short"))
	for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
		input := session.UserMessage("private body")
		input.InputOrigin = origin
		input.Pending = true
		assert.False(t, VisibleTranscriptMessage(input))
		input.Pending, input.Implicit = false, true
		assert.False(t, VisibleTranscriptMessage(input))
	}
}
