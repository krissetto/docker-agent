package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type retryingDurableSubagentStore struct {
	mu        sync.Mutex
	failures  int
	permanent error
	attempts  int
	saved     map[string]subagent.Snapshot
}

func (s *retryingDurableSubagentStore) Durability() subagent.Durability {
	return subagent.DurabilityDurable
}

func (s *retryingDurableSubagentStore) SaveTree(_ context.Context, id string, snapshot subagent.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.permanent != nil {
		return s.permanent
	}
	if s.failures > 0 {
		s.failures--
		return &session.TemporaryError{Err: errors.New("temporarily unavailable")}
	}
	if s.saved == nil {
		s.saved = map[string]subagent.Snapshot{}
	}
	s.saved[id] = snapshot
	return nil
}

func (s *retryingDurableSubagentStore) LoadTree(_ context.Context, id string) (*subagent.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, ok := s.saved[id]
	if !ok {
		return nil, nil
	}
	return &snapshot, nil
}

func TestSubagentPersistenceRetriesTemporaryDurableWrite(t *testing.T) {
	store := &retryingDurableSubagentStore{failures: 2}
	p := newSubagentPersistence(store, nil)
	p.enqueueTree("root", subagent.Snapshot{Root: "root:root"})
	require.NoError(t, p.flushNow())
	require.NoError(t, p.close())

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, 3, store.attempts)
	assert.Equal(t, subagent.NodeID("root:root"), store.saved["root"].Root)
}

func TestSubagentPersistencePermanentFailureSurfacesAndRemainsPending(t *testing.T) {
	store := &retryingDurableSubagentStore{permanent: errors.New("disk rejected write")}
	p := newSubagentPersistence(store, nil)
	p.enqueueTree("root", subagent.Snapshot{Root: "root:root"})
	err := p.flushNow()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk rejected write")

	require.Error(t, p.close())
	p.mu.Lock()
	_, pending := p.trees["root"]
	p.mu.Unlock()
	assert.True(t, pending, "failed dequeued value must not be silently lost")
}

type failingTranscriptStore struct {
	session.Store
	err error
}

func (s *failingTranscriptStore) AddSubSession(context.Context, string, *session.Session) error {
	return s.err
}

func TestSubagentPersistenceFailedTranscriptRemainsPending(t *testing.T) {
	store := &failingTranscriptStore{Store: session.NewInMemorySessionStore(), err: errors.New("transcript rejected")}
	p := newSubagentPersistence(nil, store)
	p.enqueueTranscript("parent", session.New(session.WithID("child")))
	err := p.flushNow()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transcript rejected")
	require.Error(t, p.close())
	p.mu.Lock()
	_, pending := p.transcripts["child"]
	p.mu.Unlock()
	assert.True(t, pending)
}

func TestSpawnToolSurfacesPermanentDurabilityBarrierFailure(t *testing.T) {
	store := &retryingDurableSubagentStore{permanent: errors.New("tree disk offline")}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt",
			agent.WithModel(&mockProvider{id: "test/root", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()}),
			agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(&mockProvider{id: "test/worker", stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSubagentStore(store))
	require.NoError(t, err)
	t.Cleanup(rt.subagents.Close)
	result, err := rt.handleSpawnSubagent(t.Context(), session.New(session.WithID("parent")), tools.ToolCall{Function: tools.FunctionCall{
		Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker","task":"work"}`,
	}}, nil, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, result.Output, "tree disk offline")
}

type blockingSubagentStore struct {
	mu    sync.Mutex
	trees []subagent.Snapshot
}

func (s *blockingSubagentStore) SaveTree(ctx context.Context, _ string, snapshot subagent.Snapshot) error {
	<-ctx.Done()
	s.mu.Lock()
	s.trees = append(s.trees, snapshot)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *blockingSubagentStore) LoadTree(context.Context, string) (*subagent.Snapshot, error) {
	return nil, nil
}

func TestSubagentPersistenceDoesNotBlockEnqueueOrCloseForever(t *testing.T) {
	store := &blockingSubagentStore{}
	p := newSubagentPersistence(store, nil)
	start := time.Now()
	for i := range 1000 {
		p.enqueueTree("root", subagent.Snapshot{Root: subagent.NodeID(string(rune(i%20 + 'a')))})
	}
	assert.Less(t, time.Since(start), 100*time.Millisecond)
	start = time.Now()
	err := p.close()
	assert.Less(t, time.Since(start), defaultSubagentFlushTimeout+2*time.Second)
	assert.Error(t, err)
}

func TestSubagentPersistenceCoalescesLatestTree(t *testing.T) {
	store := subagent.NewInMemoryStore()
	p := newSubagentPersistence(store, session.NewInMemorySessionStore())
	for i := range 100 {
		p.enqueueTree("root", subagent.Snapshot{Root: subagent.NodeID(string(rune(i + 1)))})
	}
	require.NoError(t, p.close())
	got, err := store.LoadTree(t.Context(), "root")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, subagent.NodeID(string(rune(100))), got.Root)
}
