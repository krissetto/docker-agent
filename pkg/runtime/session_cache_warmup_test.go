package runtime

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type warmupRequest struct {
	messages []chat.Message
	tools    []tools.Tool
}
type warmupProvider struct {
	mu       sync.Mutex
	requests []warmupRequest
	started  chan struct{}
	release  chan struct{}
}

func (p *warmupProvider) ID() modelsdev.ID        { return modelsdev.NewID("test", "warmup") }
func (p *warmupProvider) BaseConfig() base.Config { return base.Config{} }
func (p *warmupProvider) MaxTokens() int          { return 0 }
func (p *warmupProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, ts []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	n := len(p.requests)
	p.requests = append(p.requests, warmupRequest{messages: append([]chat.Message(nil), messages...), tools: append([]tools.Tool(nil), ts...)})
	p.mu.Unlock()
	if n == 0 && p.started != nil {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return newStreamBuilder().AddContent("invisible auxiliary response").AddStopWithUsage(100, 5).Build(), nil
}

func warmupFixture(t *testing.T, p provider.Provider) (*LocalRuntime, *sessionHandle, session.Store) {
	t.Helper()
	root := agent.New("root", "static instructions", agent.WithModel(p), agent.WithAddDate(true), agent.WithAddEnvironmentInfo(true), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{{Name: "example", Description: "test tool", Handler: noopHandler}}, nil)))
	store := session.NewInMemorySessionStore()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionStore(store), WithModelStore(mockModelStoreWithCost{cost: modelsdev.Cost{Input: 1, Output: 2}}), WithSessionCompaction(false), WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	handle, err := rt.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return rt, handle.(*sessionHandle), store
}

func TestCacheWarmupInvisibleAccountedAndDeduplicated(t *testing.T) {
	p := &warmupProvider{}
	_, h, store := warmupFixture(t, p)
	observation, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	h.warmSessionCache() // Also proves owner reservation does not reenter d.mu.
	h.driver.Wait()
	h.warmSessionCache()
	p.mu.Lock()
	require.Len(t, p.requests, 1)
	req := p.requests[0]
	p.mu.Unlock()
	require.NotEmpty(t, req.messages)
	assert.Equal(t, "hi", req.messages[len(req.messages)-1].Content)
	assert.Equal(t, []string{"example"}, toolNames(req.tools))
	select {
	case event := <-observation.Events:
		usage, ok := event.Event.(*TokenUsageEvent)
		require.True(t, ok)
		assert.Nil(t, usage.Usage.LastMessage)
		require.Len(t, usage.Usage.Auxiliary, 1)
		assert.InDelta(t, 0.00011, usage.Usage.Cost, 1e-12)
	case <-time.After(time.Second):
		t.Fatal("missing auxiliary accounting event")
	}
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, snapshot.MessagesSnapshot())
	assert.Empty(t, snapshot.GetLastUserMessageContent())
	require.Len(t, snapshot.AuxiliaryUsageSnapshot(), 1)
	assert.Equal(t, int64(100), snapshot.AuxiliaryUsageSnapshot()[0].Usage.InputTokens)
	assert.InDelta(t, 0.00011, snapshot.TotalCost(), 1e-12)
	assert.Equal(t, snapshot.TotalCost(), snapshot.OwnCost())
	stored, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	assert.Equal(t, snapshot.AuxiliaryUsageSnapshot(), stored.AuxiliaryUsageSnapshot())
	assert.Empty(t, stored.MessagesSnapshot())
	data, err := json.Marshal(stored)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "invisible auxiliary response")
	assert.NotContains(t, string(data), `"content":"hi"`)
}

func TestCacheWarmupDoesNotBlockRealTurnAndMatchesPrefix(t *testing.T) {
	oldConfig := paths.GetConfigDir()
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir(oldConfig) })
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte("settings:\n  cache_stable_prompts: true\n"), 0o600))
	require.True(t, userconfig.Get().CacheStablePromptsEnabled())

	p := &warmupProvider{started: make(chan struct{}), release: make(chan struct{})}
	_, h, _ := warmupFixture(t, p)
	done := make(chan struct{})
	go func() { h.warmSessionCache(); close(done) }()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("warmup deadlocked before model call")
	}
	posted := make(chan Submission, 1)
	postErr := make(chan error, 1)
	go func() {
		submission, err := h.Submit(t.Context(), TurnInput{Content: "real user turn", RequestID: "real"})
		if err != nil {
			postErr <- err
			return
		}
		posted <- submission
	}()
	var submitted Submission
	select {
	case err := <-postErr:
		require.NoError(t, err)
	case submitted = <-posted:
	case <-time.After(5 * time.Second):
		t.Fatal("real input blocked by warmup")
	}
	// AwaitTurn, unlike the driver's workgroup, does not wait for auxiliary work.
	turnCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := h.AwaitTurn(turnCtx, submitted.TurnID)
	require.NoError(t, err)
	p.mu.Lock()
	require.Len(t, p.requests, 2)
	warm, real := p.requests[0], p.requests[1]
	p.mu.Unlock()
	warmTools, err := json.Marshal(warm.tools)
	require.NoError(t, err)
	realTools, err := json.Marshal(real.tools)
	require.NoError(t, err)
	assert.JSONEq(t, string(warmTools), string(realTools))
	require.GreaterOrEqual(t, len(warm.messages), 4, "static, environment and date prefix must be present")
	for i, message := range warm.messages[:len(warm.messages)-1] {
		require.Less(t, i, len(real.messages))
		assert.Equal(t, message.Content, real.messages[i].Content, "system prefix %d", i)
		assert.Equal(t, message.Role, real.messages[i].Role)
		assert.Equal(t, message.CacheControl, real.messages[i].CacheControl)
	}
	close(p.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("warmup did not finish")
	}
	h.driver.Wait()
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.MessagesSnapshot(), 2)
	assert.Equal(t, "real user turn", snapshot.GetLastUserMessageContent())
}

type warmupMetadataGate struct {
	session.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *warmupMetadataGate) UpdateSession(ctx context.Context, value *session.Session) error {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	return s.Store.UpdateSession(ctx, value)
}

func TestCacheWarmupAccountingSurvivesDelayedRunStartSave(t *testing.T) {
	_, h, base := warmupFixture(t, &warmupProvider{})
	gate := &warmupMetadataGate{Store: base, entered: make(chan struct{}), release: make(chan struct{})}
	h.runtime.sessionStore = gate
	observer := newPersistenceObserver(gate)
	observer.owner = func(id string) *sessionDriver {
		if id == h.ID() {
			return h.driver
		}
		return nil
	}
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	saved := make(chan struct{})
	go func() { observer.OnRunStart(t.Context(), snapshot); close(saved) }()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatalf("run-start save did not enter storage: %v", observer.pendingError(h.ID()))
	}
	accounted := make(chan error, 1)
	record := session.AuxiliaryUsage{Agent: "root", Model: "test/warmup", Cost: 0.25, Usage: chat.Usage{InputTokens: 100}}
	go func() { accounted <- h.driver.recordCacheWarmupUsage(t.Context(), record, 10000, 0.8) }()
	// The delayed storage write must not hold the owner or prevent observation.
	statusCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = h.Status(statusCtx)
	require.NoError(t, err)
	select {
	case err := <-accounted:
		t.Fatalf("accounting bypassed reserved metadata write: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(gate.release)
	select {
	case <-saved:
	case <-time.After(time.Second):
		t.Fatal("run-start save did not complete")
	}
	require.NoError(t, <-accounted)
	// A later normal run-start save also retains the aggregate exactly once.
	observer.OnRunStart(t.Context(), snapshot)
	h.driver.Wait()
	stored, err := base.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	assert.Equal(t, []session.AuxiliaryUsage{record}, stored.AuxiliaryUsageSnapshot())
	assert.Equal(t, 0.25, stored.TotalCost())
	assert.Empty(t, stored.MessagesSnapshot())
}

func TestCacheWarmupSkipsImageOnlyModel(t *testing.T) {
	model := &imageOnlyProvider{imageOutputProvider: imageOutputProvider{id: "openai/gpt-image-1"}}
	_, handle, _ := warmupFixture(t, model)
	handle.warmSessionCache()
	assert.Zero(t, model.calls.Load())
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, snapshot.MessagesSnapshot())
	assert.Empty(t, snapshot.AuxiliaryUsageSnapshot())
	assert.Nil(t, snapshot.InstructionContext)
}
