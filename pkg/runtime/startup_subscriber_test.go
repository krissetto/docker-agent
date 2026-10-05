package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestStartupSubscribersCancelAndRejectUnregister(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	root := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/subscribers"}), agent.WithToolSets(&coordinatedStartToolSet{entered: entered, release: release}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithToolStartTimeout(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	t.Cleanup(func() { close(release) })
	for range 8 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { r.emitStartupTools(ctx, root, func(Event) bool { return true }); close(done) }()
		require.Eventually(t, func() bool { return r.startupRootSubscriberCount() == 1 }, time.Second, time.Millisecond)
		cancel()
		receiveWithin(t, done)
		require.Zero(t, r.startupRootSubscriberCount())
	}
	done := make(chan struct{})
	go func() { r.emitStartupTools(t.Context(), root, func(Event) bool { return false }); close(done) }()
	receiveWithin(t, done)
	require.Zero(t, r.startupRootSubscriberCount())
}

func TestStartupStalledSubscriberCannotBlockLiveReaderOrShutdown(t *testing.T) {
	const count = maxStartupToolEvents + 16
	toolsets := make([]tools.ToolSet, 0, count)
	releases := make([]chan struct{}, count)
	for i := range count {
		releases[i] = make(chan struct{})
		toolsets = append(toolsets, &coordinatedStartToolSet{entered: make(chan struct{}), release: releases[i]})
	}
	root := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/flood"}), agent.WithToolSets(toolsets...))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithToolStartTimeout(time.Hour))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	stalled, releaseSink, stalledDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		r.emitStartupTools(t.Context(), root, func(Event) bool { close(stalled); <-releaseSink; return false })
		close(stalledDone)
	}()
	receiveWithin(t, stalled)
	live := make(chan Event, count+8)
	liveDone := make(chan struct{})
	go func() {
		r.emitStartupTools(t.Context(), root, func(event Event) bool { live <- event; return true })
		close(liveDone)
	}()
	require.Eventually(t, func() bool { return r.startupRootSubscriberCount() == 2 }, time.Second, time.Millisecond)
	for _, release := range releases {
		close(release)
	}
	receiveWithin(t, liveDone)
	require.Greater(t, len(live), maxStartupToolEvents)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, r.shutdownSessions(ctx))
	require.Zero(t, r.startupRootSubscriberCount())
	close(releaseSink)
	receiveWithin(t, stalledDone)
}

func (r *LocalRuntime) startupRootSubscriberCount() int {
	r.startupToolsMu.Lock()
	defer r.startupToolsMu.Unlock()
	if seed := r.startupTools["root"]; seed != nil {
		return len(seed.subscribers)
	}
	return 0
}
