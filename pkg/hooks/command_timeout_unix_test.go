//go:build !windows && !js

package hooks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommandTimeoutBoundsDescendantPipes(t *testing.T) {
	executor := NewExecutor(&Config{ToolGuard: []MatcherConfig{{Hooks: []Hook{{Type: HookTypeCommand, Command: "sleep 2 & wait", Timeout: 1}}}}}, "", nil)
	started := time.Now()
	// The safe cancellation fallback can leave descendants alive; let our finite child exit.
	t.Cleanup(func() { <-time.After(time.Until(started.Add(2100 * time.Millisecond))) })
	result, err := executor.Dispatch(t.Context(), EventToolGuard, &Input{})
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Less(t, time.Since(started), 2*time.Second)
}

func TestCommandExitBoundsInheritedPipes(t *testing.T) {
	executor := NewExecutor(&Config{SessionStart: []Hook{{Type: HookTypeCommand, Command: "sleep 2 &", Timeout: 1}}}, "", nil)
	started := time.Now()
	// The safe cancellation fallback can leave descendants alive; let our finite child exit.
	t.Cleanup(func() { <-time.After(time.Until(started.Add(2100 * time.Millisecond))) })
	result, err := executor.Dispatch(t.Context(), EventSessionStart, &Input{})
	require.NoError(t, err)
	require.NotEmpty(t, result.SystemMessage)
	require.Less(t, time.Since(started), 1500*time.Millisecond)
}

func TestCommandOversizedOutputFailsClosed(t *testing.T) {
	executor := NewExecutor(&Config{ToolGuard: []MatcherConfig{{Hooks: []Hook{{Type: HookTypeCommand, Command: "head -c 2097152 /dev/zero", Timeout: 1}}}}}, "", nil)
	result, err := executor.Dispatch(t.Context(), EventToolGuard, &Input{})
	require.NoError(t, err)
	require.False(t, result.Allowed)
	require.Contains(t, result.Message, "capture limit")
}
