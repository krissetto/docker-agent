package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/team"
)

// These tests pin that model-level context_window is capacity only: it sets the
// limit that thresholds are fractions of, and never adds a fixed-token trigger.
// The foreground compaction_threshold and the background threshold keep
// multiplying the effective limit, and a dedicated compaction model's smaller
// window still caps that limit.

func windowPtr(n int64) *int64 { return &n }

func TestContextWindowCapacityDrivesCompactionThresholds(t *testing.T) {
	t.Parallel()

	const window = int64(100_000)
	prov := &providerOptsProvider{id: "gateway/hosted", contextWindow: windowPtr(window)}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(errorModelStore{}))
	require.NoError(t, err)

	limit := rt.effectiveContextLimit(t.Context(), root, rt.resolveContextLimit(t.Context(), prov, prov.ID()))
	require.Equal(t, window, limit, "context_window is the capacity the thresholds apply to")

	tests := []struct {
		name      string
		threshold float64
		usage     int64
		want      bool
	}{
		{"default 0.9 below", compaction.DefaultThreshold, 89_999, false},
		{"default 0.9 above", compaction.DefaultThreshold, 90_001, true},
		{"0.8 below", 0.8, 79_999, false},
		{"0.8 above", 0.8, 80_001, true},
		{"1.0 at capacity is not over", 1, 100_000, false},
		{"1.0 above capacity", 1, 100_001, true},
		// Background threshold: a fraction of the same limit, not a token count.
		{"background 0.7 below", 0.7, 69_999, false},
		{"background 0.7 above", 0.7, 70_001, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, compaction.ShouldCompact(tt.usage, 0, 0, limit, tt.threshold))
		})
	}
}

// A smaller dedicated compaction window caps the effective limit, so the
// proactive trigger fires against it. Usage that is far below the primary
// window's threshold but above the compaction window's threshold must compact.
func TestContextWindowSmallerCompactionModelCapsTrigger(t *testing.T) {
	t.Parallel()

	const (
		primaryWindow    = int64(200_000)
		compactionWindow = int64(32_000)
	)
	primary := &providerOptsProvider{id: "gateway/big", contextWindow: windowPtr(primaryWindow)}
	summarizer := &providerOptsProvider{id: "dmr/ai/qwen3", contextWindow: windowPtr(compactionWindow)}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(errorModelStore{}))
	require.NoError(t, err)

	primaryLimit := rt.resolveContextLimit(t.Context(), primary, primary.ID())
	require.Equal(t, primaryWindow, primaryLimit)

	effective := rt.effectiveContextLimit(t.Context(), root, primaryLimit)
	assert.Equal(t, compactionWindow, effective, "the smaller compaction window caps the limit")

	const usage = int64(30_000) // 0.9 of 200k is 180k; 0.9 of 32k is 28.8k
	assert.False(t, compaction.ShouldCompact(usage, 0, 0, primaryLimit, compaction.DefaultThreshold),
		"against the primary window alone this usage would not compact")
	assert.True(t, compaction.ShouldCompact(usage, 0, 0, effective, compaction.DefaultThreshold),
		"the capped limit must drive the trigger")

	model, primaryCap, compactionCap := rt.compactionCapAttribution(t.Context(), root, primary.ID())
	assert.Equal(t, "dmr/ai/qwen3", model)
	assert.Equal(t, primaryWindow, primaryCap)
	assert.Equal(t, compactionWindow, compactionCap)
	assert.True(t, compactionCaps(primaryCap, compactionCap), "the cap is attributed to the compaction model")
}

// A larger compaction window never raises the limit above the primary's, and
// the thresholds keep applying to the primary window.
func TestContextWindowLargerCompactionModelDoesNotRaiseLimit(t *testing.T) {
	t.Parallel()

	primary := &providerOptsProvider{id: "gateway/small", contextWindow: windowPtr(200_000)}
	summarizer := &providerOptsProvider{id: "gateway/huge", contextWindow: windowPtr(1_000_000)}
	root := agent.New("root", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(errorModelStore{}))
	require.NoError(t, err)

	primaryLimit := rt.resolveContextLimit(t.Context(), primary, primary.ID())
	effective := rt.effectiveContextLimit(t.Context(), root, primaryLimit)
	assert.Equal(t, int64(200_000), effective)
	assert.False(t, compactionCaps(primaryLimit, rt.compactionContextLimit(t.Context(), root)))
	assert.True(t, compaction.ShouldCompact(180_001, 0, 0, effective, compaction.DefaultThreshold))
	assert.False(t, compaction.ShouldCompact(179_999, 0, 0, effective, compaction.DefaultThreshold))
}
