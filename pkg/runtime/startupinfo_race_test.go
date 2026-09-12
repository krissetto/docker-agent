package runtime

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// TestStartupInfoResetIsSafeDuringConcurrentPerSinkEmission exercises the
// compatibility reset while many presentation consumers request seeds. Reset
// no longer gates emission; each sink must remain independently populated.
func TestStartupInfoResetIsSafeDuringConcurrentPerSinkEmission(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()

	var emissions atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			events := make(chan Event, 16)
			rt.EmitStartupInfo(t.Context(), sess, NewChannelSink(events))
			close(events)
			if len(events) > 0 {
				emissions.Add(1)
			}
			for range events {
			}
		})
		wg.Go(rt.ResetStartupInfo)
	}
	wg.Wait()
	require.Equal(t, int32(50), emissions.Load())
}

// TestStartupInfoEmittedForEverySink pins the per-consumer startup contract:
// each App/new tab has a distinct sink and must receive immediate sidebar
// information even when it shares a LocalRuntime with an existing App.
func TestStartupInfoEmittedForEverySink(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New()

	var mu sync.Mutex
	var emissions int
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			events := make(chan Event, 16)
			rt.EmitStartupInfo(t.Context(), sess, NewChannelSink(events))
			close(events)
			n := 0
			for range events {
				n++
			}
			if n > 0 {
				mu.Lock()
				emissions++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if emissions != 50 {
		t.Errorf("expected one emission for each sink, got %d", emissions)
	}
}
