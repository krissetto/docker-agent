package markdown

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMutableBlockCacheMatchesIndependentRenderer(t *testing.T) {
	inputs := []string{
		"- **bold** [link](https://example.com)\n  - nested 界 e\u0301 👩‍💻\n\n  ```go\n  /* comment\n  */ var x = 1\n  ```\n- tail",
		"1. one\n10. ten\n100. hundred\n> quote\n\nparagraph",
		"| a | b |\n| - | - |\n| short | cell |\n| much much longer | 界 |\n",
		"```go\n/* comment\ncontinued */\nvar x = `raw\nstring`\n```",
		"```text\nhello\tworld\n\n界 e\u0301 👩‍💻 https://example.com\n```",
		"```unknown-language-xyz\nplain\ntext\n```",
		"paragraph with **unfinished\nformatting** [link](https://example.com) e\u0301 👩‍💻",
	}
	for i, input := range inputs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r := NewIncrementalRenderer(41)
			for end := range len(input) + 1 {
				if end%17 == 0 {
					r.SetWidth(23 + end%3*19)
				}
				got, blocks, err := r.RenderWithCodeBlocks(input[:end])
				require.NoError(t, err)
				want, wantBlocks, err := NewFastRenderer(r.width).RenderWithCodeBlocks(input[:end])
				require.NoError(t, err)
				require.Equal(t, want, got, "byte %d", end)
				require.Equal(t, wantBlocks, blocks)
				require.LessOrEqual(t, r.stream.bytes, streamCacheBudget)
			}
			replacement := "- edited\n- content"
			got, err := r.Render(replacement)
			require.NoError(t, err)
			want, err := NewFastRenderer(r.width).Render(replacement)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestStreamingSyntaxVariantsDoNotEnterGlobalCache(t *testing.T) {
	syntaxHighlightCacheMu.Lock()
	before := syntaxHighlightCache.Len()
	syntaxHighlightCacheMu.Unlock()
	r := NewIncrementalRenderer(80)
	input := "```go\n"
	for i := range 160 {
		input += fmt.Sprintf("var streamingUnique%d = %d\n", i, i)
		_, err := r.RenderParts(input)
		require.NoError(t, err)
	}
	syntaxHighlightCacheMu.Lock()
	after := syntaxHighlightCache.Len()
	syntaxHighlightCacheMu.Unlock()
	require.Equal(t, before, after)
	syntaxVariants := 0
	for key := range r.stream.current {
		if key.kind == 4 {
			syntaxVariants++
		}
	}
	require.Equal(t, 1, syntaxVariants)
	r.Reset()
	require.Empty(t, r.stream.current)
	require.Empty(t, r.stream.previous)
}

func TestStreamCachePrunesOldOperationsAndBoundsBytes(t *testing.T) {
	cache := streamCache{}
	cache.begin()
	cache.keep(streamKey{kind: 2, text: "old"}, "value", 5)
	cache.end()
	cache.begin()
	cache.keep(streamKey{kind: 2, text: "new"}, "other", 5)
	cache.end()
	require.Len(t, cache.current, 1)
	_, ok := cache.get(streamKey{kind: 2, text: "old"})
	require.False(t, ok)
	cache.begin()
	cache.keep(streamKey{kind: 2, text: strings.Repeat("x", streamCacheBudget)}, "value", 5)
	cache.end()
	require.Empty(t, cache.current)
}
