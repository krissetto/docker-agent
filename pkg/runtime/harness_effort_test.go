package runtime

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func newClaudeCodeEffortRuntime(t *testing.T) (*LocalRuntime, *agent.Agent) {
	t.Helper()
	root := agent.New("root", "test", agent.WithHarness(&latest.HarnessConfig{Type: "claude-code"}))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(emptyCatalogStore{}),
		WithModelSwitcherConfig(&ModelSwitcherConfig{
			ProviderRegistry: testProviderRegistry(),
			EnvProvider:      environment.NewMapEnvProvider(map[string]string{}),
		}),
	)
	require.NoError(t, err)
	return r, root
}

func claudeCodeSession(t *testing.T, r *LocalRuntime) *sessionHandle {
	t.Helper()
	handle, err := r.CreateSession(t.Context(), session.New(session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return handle.(*sessionHandle)
}

// TestHarnessEffortIsSessionScoped pins that /effort and Shift+Tab on a harness
// agent change only the calling session: the sibling session and the shared
// agent configuration keep the configured effort.
func TestHarnessEffortIsSessionScoped(t *testing.T) {
	t.Parallel()

	r, root := newClaudeCodeEffortRuntime(t)
	first := claudeCodeSession(t, r)
	second := claudeCodeSession(t, r)

	assert.True(t, first.Metadata().Capabilities.ThinkingLevels, "a claude-code harness advertises effort levels")
	assert.Equal(t, []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}, first.ThinkingLevels(t.Context()))
	assert.Equal(t, effort.Level(""), first.CurrentThinkingLevel(t.Context()), "the CLI picks the effort until a session sets one")

	level, err := first.CycleThinkingLevel(t.Context())
	require.NoError(t, err)
	assert.Equal(t, effort.Low, level, "cycling from auto starts at the first level")

	applied, err := first.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	assert.Equal(t, effort.High, applied)
	assert.Equal(t, effort.High, first.CurrentThinkingLevel(t.Context()))

	assert.Equal(t, effort.Level(""), second.CurrentThinkingLevel(t.Context()), "a sibling session keeps its own effort")
	assert.Empty(t, root.Harness().Effort, "the shared harness configuration is never mutated")
}

// TestHarnessEffortReachesTheRunOnlyForItsSession checks the run path: the
// session's scoped context yields a harness config carrying its effort, while
// the agent's own configuration (and the harness session key) stay unchanged.
func TestHarnessEffortReachesTheRunOnlyForItsSession(t *testing.T) {
	t.Parallel()

	r, root := newClaudeCodeEffortRuntime(t)
	chosen := claudeCodeSession(t, r)
	other := claudeCodeSession(t, r)

	_, err := chosen.SetThinkingLevel(t.Context(), effort.Max)
	require.NoError(t, err)

	assert.Equal(t, "max", harnessConfigFor(chosen.driver.scopeModels(t.Context()), root).Effort)
	assert.Empty(t, harnessConfigFor(other.driver.scopeModels(t.Context()), root).Effort)
	assert.Empty(t, harnessConfigFor(t.Context(), root).Effort)
	assert.Equal(t, root.Harness(), harnessConfigFor(t.Context(), root), "without a session override the agent's own config is used")
	assert.Equal(t, "claude-code", harnessLabel(root.Harness()), "the harness session key ignores the effort")
}

// TestHarnessEffortRejectsStaleBinding checks the stale guard: a choice made
// against a binding that has since changed is rejected, and the newer choice stays.
func TestHarnessEffortRejectsStaleBinding(t *testing.T) {
	t.Parallel()

	r, _ := newClaudeCodeEffortRuntime(t)
	h := claudeCodeSession(t, r)

	stale := h.driver.activeBinding()
	_, err := h.SetThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)

	err = h.driver.setHarnessEffort(t.Context(), stale, effort.Max)
	require.Error(t, err)
	assert.Equal(t, effort.High, h.CurrentThinkingLevel(t.Context()), "a rejected stale choice must not overwrite the current effort")
}

// TestHarnessEffortRejectsUnsupportedLevel keeps the harness vocabulary to the
// levels harness config validation accepts.
func TestHarnessEffortRejectsUnsupportedLevel(t *testing.T) {
	t.Parallel()

	r, _ := newClaudeCodeEffortRuntime(t)
	h := claudeCodeSession(t, r)

	_, err := h.SetThinkingLevel(t.Context(), effort.Minimal)
	require.Error(t, err)
	assert.Equal(t, effort.Level(""), h.CurrentThinkingLevel(t.Context()))
}

// TestHarnessAgentInfoShowsSessionEffort checks the sidebar projection: the
// session's effort, or "adaptive" while the CLI picks it.
func TestHarnessAgentInfoShowsSessionEffort(t *testing.T) {
	t.Parallel()

	r, _ := newClaudeCodeEffortRuntime(t)
	h := claudeCodeSession(t, r)

	_, teamInfo := collectAgentInfo(func(sink EventSink) { h.EmitPinnedAgentInfo(t.Context(), sink) })
	require.NotNil(t, teamInfo)
	require.Len(t, teamInfo.AvailableAgents, 1)
	assert.Equal(t, "adaptive", teamInfo.AvailableAgents[0].Thinking)
	assert.True(t, teamInfo.AvailableAgents[0].CanCycleThinking)

	_, err := h.SetThinkingLevel(t.Context(), effort.Max)
	require.NoError(t, err)
	_, teamInfo = collectAgentInfo(func(sink EventSink) { h.EmitPinnedAgentInfo(t.Context(), sink) })
	require.NotNil(t, teamInfo)
	assert.Equal(t, "max", teamInfo.AvailableAgents[0].Thinking)
}

// TestHarnessEffortConcurrentSessionsStayIsolated drives two sessions of the
// same harness agent from concurrent goroutines. Each keeps its own choice, and
// the shared configuration is never written, so -race stays quiet.
func TestHarnessEffortConcurrentSessionsStayIsolated(t *testing.T) {
	t.Parallel()

	r, root := newClaudeCodeEffortRuntime(t)
	first := claudeCodeSession(t, r)
	second := claudeCodeSession(t, r)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := first.SetThinkingLevel(t.Context(), effort.High)
			assertStaleOrNil(t, err)
			_ = first.CurrentThinkingLevel(t.Context())
		}()
		go func() {
			defer wg.Done()
			_, err := second.CycleThinkingLevel(t.Context())
			assertStaleOrNil(t, err)
			_ = harnessConfigFor(second.driver.scopeModels(t.Context()), root)
		}()
	}
	wg.Wait()

	assert.Equal(t, effort.High, first.CurrentThinkingLevel(t.Context()))
	assert.Contains(t, harnessEffortLevels, second.CurrentThinkingLevel(t.Context()), "twenty cycles leave the second session on a harness level")
	assert.Empty(t, root.Harness().Effort, "the shared configuration stays untouched under concurrent sessions")
}

// assertStaleOrNil accepts a concurrent mutation that lost the binding race:
// the stale guard rejects it, as it does for a concurrent model override.
func assertStaleOrNil(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var sessionErr *SessionError
	if assert.ErrorAs(t, err, &sessionErr) {
		assert.Equal(t, SessionErrorStale, sessionErr.Kind)
	}
}

// TestHarnessEffortRunConfigIsACopy checks that the run configuration is a copy:
// editing it never reaches the agent's own configuration.
func TestHarnessEffortRunConfigIsACopy(t *testing.T) {
	t.Parallel()

	r, root := newClaudeCodeEffortRuntime(t)
	h := claudeCodeSession(t, r)
	_, err := h.SetThinkingLevel(t.Context(), effort.Max)
	require.NoError(t, err)

	scoped := harnessConfigFor(h.driver.scopeModels(t.Context()), root)
	assert.NotSame(t, root.Harness(), scoped)
	scoped.Effort = "low"
	assert.Empty(t, root.Harness().Effort)
}
