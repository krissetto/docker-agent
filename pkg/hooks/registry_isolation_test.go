package hooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryCloneRebindsBuiltinFactory(t *testing.T) {
	source := NewRegistry()
	require.NoError(t, source.RegisterBuiltin("custom", func(context.Context, *Input, []string) (*Output, error) { return &Output{SystemMessage: "source"}, nil }))
	copy := source.Clone()
	require.NoError(t, copy.RegisterBuiltin("custom", func(context.Context, *Input, []string) (*Output, error) { return &Output{SystemMessage: "copy"}, nil }))
	for _, tc := range []struct {
		registry *Registry
		want     string
	}{{source, "source"}, {copy, "copy"}} {
		factory, ok := tc.registry.Lookup(HookTypeBuiltin)
		require.True(t, ok)
		handler, err := factory(HandlerEnv{}, Hook{Command: "custom"})
		require.NoError(t, err)
		result, err := handler.Run(t.Context(), []byte(`{}`))
		require.NoError(t, err)
		require.Equal(t, tc.want, result.Output.SystemMessage)
	}
	require.NoError(t, source.RegisterBuiltin("later", func(context.Context, *Input, []string) (*Output, error) { return nil, nil }))
	_, exists := copy.LookupBuiltin("later")
	require.False(t, exists)
}

func TestRegistryDefaultsPreserveOverrides(t *testing.T) {
	registry := NewRegistry()
	override := func(context.Context, *Input, []string) (*Output, error) {
		return &Output{SystemMessage: "override"}, nil
	}
	require.NoError(t, registry.RegisterBuiltin("custom", override))
	defaults := NewRegistry()
	require.NoError(t, defaults.RegisterBuiltin("custom", func(context.Context, *Input, []string) (*Output, error) { return nil, nil }))
	registry.RegisterDefaults(defaults)
	fn, ok := registry.LookupBuiltin("custom")
	require.True(t, ok)
	out, err := fn(t.Context(), &Input{}, nil)
	require.NoError(t, err)
	require.Equal(t, "override", out.SystemMessage)
}
