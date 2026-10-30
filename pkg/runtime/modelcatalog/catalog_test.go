package modelcatalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecorateModelChoices(t *testing.T) {
	t.Parallel()

	t.Run("default marked current when no override is set", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"",
			nil,
		)
		require.Len(t, got, 2)
		assert.True(t, got[0].IsCurrent, "the IsDefault model must be marked IsCurrent when no override is set")
		assert.False(t, got[1].IsCurrent)
	})

	t.Run("override matching a known choice marks it current", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{
				{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
				{Name: "other", Ref: "openai/gpt-4o"},
			},
			"openai/gpt-4o",
			nil,
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when an override is active")
		assert.True(t, got[1].IsCurrent)
	})

	t.Run("synthesizes choice for inline override not in list", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"anthropic/claude-sonnet-4-0",
			nil,
		)
		require.Len(t, got, 2)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[1].Ref)
		assert.Equal(t, "anthropic", got[1].Provider)
		assert.Equal(t, "claude-sonnet-4-0", got[1].Model)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	t.Run("appends custom refs from session history", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o", "anthropic/claude-sonnet-4-0"},
		)
		require.Len(t, got, 3)
		assert.Equal(t, "openai/gpt-4o-mini", got[0].Ref)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCustom)
		assert.Equal(t, "anthropic/claude-sonnet-4-0", got[2].Ref)
		assert.True(t, got[2].IsCustom)
	})

	t.Run("does not duplicate custom ref already in list", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "openai/gpt-4o", IsDefault: true}},
			"",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 1)
		assert.Equal(t, "openai/gpt-4o", got[0].Ref)
	})

	t.Run("non-provider override does not synthesize a fabricated choice", func(t *testing.T) {
		t.Parallel()
		// "my_model" is a config key (no slash); when not in the runtime's
		// list we should NOT fabricate a choice for it because we have no
		// provider/model breakdown to display.
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"my_model",
			nil,
		)
		require.Len(t, got, 1)
		assert.False(t, got[0].IsCurrent, "default must not be marked current when override is unknown")
	})

	t.Run("custom ref matching the active override is marked current", func(t *testing.T) {
		t.Parallel()
		got := DecorateModelChoices(
			[]ModelChoice{{Name: "default", Ref: "default", IsDefault: true}},
			"openai/gpt-4o",
			[]string{"openai/gpt-4o"},
		)
		require.Len(t, got, 2)
		assert.False(t, got[0].IsCurrent)
		assert.Equal(t, "openai/gpt-4o", got[1].Ref)
		assert.True(t, got[1].IsCurrent)
		assert.True(t, got[1].IsCustom)
	})

	// AvailableModels implementations may return a slice backed by an
	// internal cache; mutating its IsCurrent flag in place would leak
	// state across sessions. The function must therefore never modify
	// the input slice or its underlying array.
	t.Run("does not mutate the input slice", func(t *testing.T) {
		t.Parallel()
		input := []ModelChoice{
			{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
			{Name: "other", Ref: "openai/gpt-4o"},
		}
		orig := make([]ModelChoice, len(input))
		copy(orig, input)

		_ = DecorateModelChoices(input, "openai/gpt-4o", []string{"anthropic/claude-sonnet-4-0"})

		assert.Equal(t, orig, input, "DecorateModelChoices must not modify the input slice")
	})
}
