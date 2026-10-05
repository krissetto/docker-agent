package hooks

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type callbackHandler func(context.Context, []byte) (HandlerResult, error)

func (fn callbackHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	return fn(ctx, input)
}

func TestCallbackFailuresFailClosedInsideDispatchWorkers(t *testing.T) {
	for _, kind := range []string{"factory panic", "handler panic", "builtin panic", "nil factory", "nil handler", "unknown type", "empty type"} {
		t.Run(kind, func(t *testing.T) {
			registry := NewRegistry()
			hook := Hook{Type: "custom", Command: "crash"}
			switch kind {
			case "factory panic":
				registry.Register("custom", func(HandlerEnv, Hook) (Handler, error) { panic("factory failed") })
			case "handler panic":
				registry.Register("custom", func(HandlerEnv, Hook) (Handler, error) {
					return callbackHandler(func(context.Context, []byte) (HandlerResult, error) { panic("handler failed") }), nil
				})
			case "builtin panic":
				hook.Type = HookTypeBuiltin
				require.NoError(t, registry.RegisterBuiltin("crash", func(context.Context, *Input, []string) (*Output, error) { panic("builtin failed") }))
			case "nil factory":
				registry.Register("custom", nil)
			case "nil handler":
				registry.Register("custom", func(HandlerEnv, Hook) (Handler, error) { return nil, nil })
			case "empty type":
				hook.Type = ""
			}
			executor := NewExecutorWithRegistry(&Config{ToolGuard: []MatcherConfig{{Hooks: []Hook{hook}}}}, "", nil, registry)
			result, err := executor.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell"})
			require.NoError(t, err)
			require.False(t, result.Allowed)
			require.Equal(t, -1, result.ExitCode)
		})
	}
}

func TestInvalidMatcherCannotRemoveGuard(t *testing.T) {
	for _, config := range []MatcherConfig{{Matcher: "[", Hooks: []Hook{{Type: HookTypeCommand, Command: "exit 2"}}}, {Matcher: "*"}} {
		executor := NewExecutor(&Config{ToolGuard: []MatcherConfig{config}}, "", nil)
		require.True(t, executor.Has(EventToolGuard))
		_, err := executor.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell"})
		require.Error(t, err)
	}
}

func TestFactoryAndBuiltinMutableInputsAreDetached(t *testing.T) {
	registry := NewRegistry()
	config := &Config{SessionStart: []Hook{{Type: "custom", Args: []string{"original"}, Env: map[string]string{"KEY": "original"}}}}
	registry.Register("custom", func(env HandlerEnv, hook Hook) (Handler, error) {
		require.Equal(t, "original", hook.Args[0])
		require.Equal(t, "original", hook.Env["KEY"])
		require.Equal(t, "KEY=original", env.Env[0])
		hook.Args[0], hook.Env["KEY"], env.Env[0] = "changed", "changed", "changed"
		return callbackHandler(func(_ context.Context, input []byte) (HandlerResult, error) {
			input[0] = '!'
			return HandlerResult{}, nil
		}), nil
	})
	env := []string{"KEY=original"}
	executor := NewExecutorWithRegistry(config, "", env, registry)
	config.SessionStart[0].Args[0], env[0] = "caller changed", "caller changed"
	for range 2 {
		_, err := executor.Dispatch(t.Context(), EventSessionStart, &Input{})
		require.NoError(t, err)
	}
	require.NoError(t, registry.RegisterBuiltin("mutate", func(_ context.Context, _ *Input, args []string) (*Output, error) {
		require.Equal(t, "original", args[0])
		args[0] = "changed"
		return nil, nil
	}))
	factory, _ := registry.Lookup(HookTypeBuiltin)
	handler, err := factory(HandlerEnv{}, Hook{Command: "mutate", Args: []string{"original"}})
	require.NoError(t, err)
	for range 2 {
		_, err := handler.Run(t.Context(), []byte(`{}`))
		require.NoError(t, err)
	}
}

func TestCancellationJoinsHookCallback(t *testing.T) {
	registry := NewRegistry()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	require.NoError(t, registry.RegisterBuiltin("wait", func(context.Context, *Input, []string) (*Output, error) {
		close(started)
		<-release
		return nil, nil
	}))
	executor := NewExecutorWithRegistry(&Config{SessionStart: []Hook{{Type: HookTypeBuiltin, Command: "wait"}}}, "", nil, registry)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer close(done)
		_, _ = executor.Dispatch(ctx, EventSessionStart, &Input{})
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("dispatch abandoned its callback")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not join its callback")
	}
}

func TestCommandCapturedOutputIsBounded(t *testing.T) {
	var output hookOutputBuffer
	payload := make([]byte, maxHookOutputBytes+1)
	for range 3 {
		n, err := output.Write(payload)
		require.NoError(t, err)
		require.Len(t, payload, n)
	}
	require.Equal(t, maxHookOutputBytes, output.Len())
}
