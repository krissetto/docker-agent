package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// mapModelStore returns a distinct context limit per model id, so a test can
// give the primary model and the dedicated compaction model different windows
// (mockModelStoreWithLimit returns one limit for every id, which can't tell
// the two models apart).
type mapModelStore struct {
	ModelStore

	limits map[string]int
}

func (m mapModelStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	if lim, ok := m.limits[id.String()]; ok {
		return &modelsdev.Model{Limit: modelsdev.Limit{Context: lim}, Cost: &modelsdev.Cost{}}, nil
	}
	return nil, nil
}

func lastTokenUsage(events []Event) *TokenUsageEvent {
	var last *TokenUsageEvent
	for _, ev := range events {
		if e, ok := ev.(*TokenUsageEvent); ok {
			last = e
		}
	}
	return last
}

func countCompactionStarts(events []Event) int {
	var n int
	for _, ev := range events {
		if e, ok := ev.(*SessionCompactionEvent); ok && e.Status == "started" {
			n++
		}
	}
	return n
}

func drainRunStream(t *testing.T, rt *LocalRuntime, sess *session.Session) []Event {
	t.Helper()
	var events []Event
	for ev := range rt.runExecution(t.Context(), sess) {
		events = append(events, ev)
	}
	return events
}

func firstAgentInfo(events []Event) *AgentInfoEvent {
	for _, ev := range events {
		if e, ok := ev.(*AgentInfoEvent); ok {
			return e
		}
	}
	return nil
}

// startupAgentInfo drives EmitStartupInfo (the emitAgentAndTeamInfo call site
// used on session start/restore) and returns the resulting AgentInfoEvent.
func startupAgentInfo(t *testing.T, rt *LocalRuntime) *AgentInfoEvent {
	t.Helper()
	events := make(chan Event, 20)
	rt.EmitStartupInfo(t.Context(), nil, NewChannelSink(events))
	close(events)

	var collected []Event
	for ev := range events {
		collected = append(collected, ev)
	}
	return firstAgentInfo(collected)
}

// TestEmitStartupInfo_AgentInfoAttributesCappedLimit verifies that the
// sidebar's AgentInfoEvent carries the compaction-model cap attribution only
// when the compaction model actually reduces the effective limit below the
// primary model's own window. Issue #3241 follow-up.
func TestEmitStartupInfo_AgentInfoAttributesCappedLimit(t *testing.T) {
	t.Parallel()

	store := mapModelStore{limits: map[string]int{"primary/big": 200_000, "compaction/small": 16_000, "compaction/big": 200_000}}

	t.Run("capped: attribution present", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big"}
		compactionModel := &mockProvider{id: "compaction/small"}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compactionModel))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(store))
		require.NoError(t, err)

		info := startupAgentInfo(t, rt)
		require.NotNil(t, info)
		assert.Equal(t, int64(16_000), info.ContextLimit)
		assert.Equal(t, "compaction/small", info.CompactionModel)
		assert.Equal(t, int64(200_000), info.PrimaryContextLimit)
	})

	t.Run("not capped: attribution absent", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big"}
		compactionModel := &mockProvider{id: "compaction/big"}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compactionModel))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(store))
		require.NoError(t, err)

		info := startupAgentInfo(t, rt)
		require.NotNil(t, info)
		assert.Equal(t, int64(200_000), info.ContextLimit)
		assert.Empty(t, info.CompactionModel)
		assert.Zero(t, info.PrimaryContextLimit)
	})

	t.Run("no compaction model: attribution absent", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big"}
		root := agent.New("root", "test", agent.WithModel(primary))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithCurrentAgent("root"), WithModelStore(store))
		require.NoError(t, err)

		info := startupAgentInfo(t, rt)
		require.NotNil(t, info)
		assert.Equal(t, int64(200_000), info.ContextLimit)
		assert.Empty(t, info.CompactionModel)
		assert.Zero(t, info.PrimaryContextLimit)
	})
}

// TestRunStream_GaugeUsesEffectiveContextLimit verifies end-to-end that the UI
// context gauge (TokenUsageEvent) reports the EFFECTIVE window — the primary
// window capped to a smaller dedicated compaction model's window — so the bar
// fills to ~90% exactly as the (now earlier) compaction trigger fires. Issue
// #3241. With no compaction model the gauge still reports the primary window.
func TestRunStream_GaugeUsesEffectiveContextLimit(t *testing.T) {
	t.Parallel()

	store := mapModelStore{limits: map[string]int{"primary/big": 200_000, "compaction/small": 16_000}}

	t.Run("capped to compaction model window", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(3, 2).Build()}
		compaction := &mockProvider{id: "compaction/small"}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(store))
		require.NoError(t, err)

		usage := lastTokenUsage(drainRunStream(t, rt, session.New(session.WithUserMessage("Hi"))))
		require.NotNil(t, usage)
		require.NotNil(t, usage.Usage)
		assert.Equal(t, int64(16_000), usage.Usage.ContextLimit,
			"gauge must report the effective (capped) window, not the primary's 200k")
	})

	t.Run("primary window when no compaction model", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(3, 2).Build()}
		root := agent.New("root", "test", agent.WithModel(primary))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(store))
		require.NoError(t, err)

		usage := lastTokenUsage(drainRunStream(t, rt, session.New(session.WithUserMessage("Hi"))))
		require.NotNil(t, usage)
		require.NotNil(t, usage.Usage)
		assert.Equal(t, int64(200_000), usage.Usage.ContextLimit)
	})
}

// TestRunStream_CompactionFiresAtEffectiveContextLimit verifies end-to-end that
// a smaller dedicated compaction model makes proactive compaction fire while the
// session still fits the compaction model's window: usage below 90% of the
// primary window but above 90% of the compaction window triggers a compaction,
// whereas the identical usage with no compaction model does not. Issue #3241.
func TestRunStream_CompactionFiresAtEffectiveContextLimit(t *testing.T) {
	t.Parallel()

	store := mapModelStore{limits: map[string]int{"primary/big": 200_000, "compaction/small": 16_000}}

	// 15_000 tokens: above 90% of the 16k compaction window (14_400), below 90%
	// of the 200k primary window (180_000).
	const seeded = 15_000

	t.Run("fires against the smaller compaction window", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(3, 2).Build()}
		// The compaction model serves the summary sub-run, so it needs a real
		// stream that produces a summary.
		compaction := &mockProvider{id: "compaction/small", stream: newStreamBuilder().AddContent("summary").AddStopWithUsage(1, 1).Build()}
		root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(compaction))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(store))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("Hi"))
		sess.InputTokens = seeded

		assert.Positive(t, countCompactionStarts(drainRunStream(t, rt, sess)),
			"compaction must fire when usage exceeds 90%% of the compaction model's window")
	})

	t.Run("does not fire without a compaction model", func(t *testing.T) {
		t.Parallel()
		primary := &mockProvider{id: "primary/big", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(3, 2).Build()}
		root := agent.New("root", "test", agent.WithModel(primary))
		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(true), WithModelStore(store))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("Hi"))
		sess.InputTokens = seeded

		assert.Zero(t, countCompactionStarts(drainRunStream(t, rt, sess)),
			"the same usage stays well under 90%% of the primary window, so no compaction")
	})
}
