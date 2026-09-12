package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// Attach protocol: user-message events whose session position is below the
// transcript snapshot end are already on screen from the snapshot and must
// be dropped; events at/past the snapshot end (or without a position) render.
func TestAttachBarrierDedupesUserMessages(t *testing.T) {
	t.Parallel()

	newPage := func(snapshotEnd int) *chatPage {
		return &chatPage{
			snapshotEnd: snapshotEnd,
			messages:    messages.New(animation.NewRuntime(), &service.SessionState{}),
		}
	}

	// Inside the snapshot: dropped (handled, no command, nothing rendered).
	p := newPage(5)
	handled, cmd := p.handleRuntimeEvent(&runtime.UserMessageEvent{Message: "old", SessionPosition: 3})
	assert.True(t, handled)
	assert.Nil(t, cmd, "event inside the snapshot is dropped without touching the view")

	// At/past the snapshot end: rendered.
	handled, cmd = p.handleRuntimeEvent(&runtime.UserMessageEvent{Message: "new", SessionPosition: 5})
	assert.True(t, handled)
	assert.NotNil(t, cmd)

	// Unstamped positions always render (no basis to dedupe).
	handled, cmd = p.handleRuntimeEvent(&runtime.UserMessageEvent{Message: "unstamped", SessionPosition: -1})
	assert.True(t, handled)
	assert.NotNil(t, cmd)

	// No snapshot (regular tabs): everything renders.
	p = newPage(0)
	handled, cmd = p.handleRuntimeEvent(&runtime.UserMessageEvent{Message: "any", SessionPosition: 0})
	assert.True(t, handled)
	assert.NotNil(t, cmd)
}
