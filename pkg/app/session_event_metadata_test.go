package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
)

func newMetadataTestApp(t *testing.T) *App {
	t.Helper()
	return New(t.Context(), nil, nil, runtime.SessionBinding{}, WithRuntimeServices(nil))
}

func TestSubscribeWithUnwrapsSessionMetadataForCompatibility(t *testing.T) {
	t.Parallel()
	a := newMetadataTestApp(t)
	got := make(chan any, 1)
	go a.SubscribeWith(t.Context(), func(msg tea.Msg) { got <- msg })
	event := runtime.StreamStarted("s", "root")
	a.events <- SessionEventMsg{Event: event, Seed: true}
	assert.Equal(t, event, <-got)
}

func TestSubscribeWithSessionMetadataPreservesSeedFlag(t *testing.T) {
	t.Parallel()
	a := newMetadataTestApp(t)
	got := make(chan any, 1)
	go a.Subscribe(t.Context(), func(msg any) { got <- msg }, SubscribeOptions{PreserveSessionMetadata: true})
	event := runtime.StreamStarted("s", "root")
	a.events <- SessionEventMsg{Event: event, Seed: true}
	assert.Equal(t, SessionEventMsg{Event: event, Seed: true}, <-got)
}
