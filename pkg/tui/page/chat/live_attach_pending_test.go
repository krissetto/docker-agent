package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestReplayStreamStartedShowsAttachedPending(t *testing.T) {
	t.Parallel()

	state := &service.SessionState{}
	page := &chatPage{messages: messagecomponent.New(animation.NewRuntime(), state), sidebar: sidebar.New(animation.NewRuntime(), t.Context(), state)}
	before := page.messages.View()

	// Supervisor maps a reconnect replay's nonzero sequence to Seed=false.
	page.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: runtime.StreamStarted("replay-child", "worker"), Seed: false})
	assert.NotEqual(t, before, page.messages.View(), "replayed genuine start must show attached pending")
}

func TestOrdinaryStartShowsPendingEvenWhenSeedMarked(t *testing.T) {
	t.Parallel()

	state := &service.SessionState{}
	page := &chatPage{messages: messagecomponent.New(animation.NewRuntime(), state), sidebar: sidebar.New(animation.NewRuntime(), t.Context(), state)}
	before := page.messages.View()
	page.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: runtime.StreamStarted("root", "root"), Seed: true})
	assert.NotEqual(t, before, page.messages.View())
}
