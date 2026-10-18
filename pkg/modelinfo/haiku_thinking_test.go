package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/effort"
)

func TestHaikuThinkingCapabilities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model    string
		adaptive bool
	}{
		{"claude-haiku-5-5", true},
		{"claude-haiku-5.5", true},
		{"claude-haiku-5-5-latest", true},
		{"claude-haiku-5-5-20260901", true},
		{"claude-haiku-5-5@default", true},
		{"anthropic/claude-haiku-5.5", true},
		{"openrouter/anthropic/claude-haiku-5-5", true},
		{"anthropic.claude-haiku-5-5", true},
		{"us.anthropic.claude-haiku-5-5-v1:0", true},
		{"global.anthropic.claude-haiku-5-5", true},
		{" CLAUDE-HAIKU-5-5 ", true},
		{"claude-haiku-5-6", true},
		{"claude-haiku-6", true},
		{"claude-haiku-4-5", false},
		{"claude-haiku-4.5-20251001", false},
		{"claude-haiku-4-6", false},
		{"claude-haiku-5", false},
		{"claude-haiku-5-4", false},
		{"claude-haiku-5-20260901", false},
		{"claude-3-haiku-20240307", false},
		{"claude-3-5-haiku-latest", false},
		{"claude-haiku-5-5foo", false},
		{"claude-haiku-6foo", false},
		{"claude-haiku-555", false},
		{"openai/claude-haiku-5-5", false},
		{"not-claude-haiku-5-5", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.adaptive, UsesAdaptiveThinkingByDefault(tc.model))
			assert.Equal(t, tc.adaptive, SupportsAdaptiveThinking(tc.model))
			assert.Equal(t, tc.adaptive, RejectsTokenThinking(tc.model))
			assert.Equal(t, !tc.adaptive, SupportsFullThinkingDisplay(tc.model))
			want := []effort.Level{effort.None, effort.Low, effort.Medium, effort.High}
			if tc.adaptive {
				want = append(want, effort.XHigh, effort.Max)
			}
			if tc.model == "openai/claude-haiku-5-5" {
				want = []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High}
			}
			assert.Equal(t, want, SupportedThinkingLevels("anthropic", tc.model))
		})
	}
}
