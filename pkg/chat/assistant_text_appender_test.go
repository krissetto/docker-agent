package chat

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAssistantTextAppenderMatchesAppendAssistantPart(t *testing.T) {
	steps := []AssistantPart{
		{Type: AssistantPartContent, Text: "he"},
		{Type: AssistantPartContent, Text: "llo"},
		{Type: AssistantPartReasoning, Text: "think"},
		{Type: AssistantPartReasoning, Text: "ing"},
		{Type: AssistantPartContent, Text: ""},
		{Type: AssistantPartToolCall, ToolCallID: "call-1"},
		{Type: AssistantPartContent, Text: "after"},
		{Type: AssistantPartContent, Text: "!"},
		{Type: AssistantPartToolCall, ToolCallID: "call-1"},
		{Type: AssistantPartReasoning, Text: "again"},
		{Type: AssistantPartContent, Text: "tail"},
	}
	var appender AssistantTextAppender
	var got, want []AssistantPart
	for _, step := range steps {
		got = appender.Append(got, step)
		want = AppendAssistantPart(want, step)
		require.Equal(t, want, got)
	}
}

func TestAssistantTextAppenderKeepsEmittedTextImmutable(t *testing.T) {
	var appender AssistantTextAppender
	var parts []AssistantPart
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartContent, Text: "abc"})
	first := parts[0].Text
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartReasoning, Text: "r1"})
	second := parts[1].Text
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartReasoning, Text: "r2"})
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartContent, Text: "defgh"})
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartToolCall, ToolCallID: "call"})
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartContent, Text: "ij"})
	require.Equal(t, "abc", first)
	require.Equal(t, "r1", second)
	require.Equal(t, "abc", parts[0].Text)
	require.Equal(t, "r1r2", parts[1].Text)
	require.Equal(t, "defgh", parts[2].Text)
	require.Equal(t, "ij", parts[4].Text)
}

func TestAssistantTextAppenderReseedsAfterReplacement(t *testing.T) {
	var appender AssistantTextAppender
	var parts []AssistantPart
	parts = appender.Append(parts, AssistantPart{Type: AssistantPartContent, Text: "abc"})
	replaced := slices.Clone(parts)
	replaced[0].Text = "zz"
	parts = appender.Append(replaced, AssistantPart{Type: AssistantPartContent, Text: "d"})
	require.Equal(t, []AssistantPart{{Type: AssistantPartContent, Text: "zzd"}}, parts)

	fresh := []AssistantPart{{Type: AssistantPartContent, Text: "qq"}}
	parts = appender.Append(fresh, AssistantPart{Type: AssistantPartContent, Text: "r"})
	require.Equal(t, []AssistantPart{{Type: AssistantPartContent, Text: "qqr"}}, parts)
}

func BenchmarkAssistantTextAppenderDeltas(b *testing.B) {
	var appender AssistantTextAppender
	var parts []AssistantPart
	chunk := AssistantPart{Type: AssistantPartContent, Text: strings.Repeat("x", 16)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		parts = appender.Append(parts, chunk)
	}
}
