package markdown

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParserCheckpointsMatchFullRendererAtEveryByte(t *testing.T) {
	cases := []string{
		"### Heading\n- first **item**\n### Next\n- second `item`\n",
		"- parent\n  - nested\n\n  continued\n  ```go\n  x := 1\n  ```\n# Next\nbody",
		"> quote\n> ```\n> code\n> ```\n\nparagraph\n# Heading\nnext",
		"Intro\n\n| left | right |\n| --- | --- |\n| short | long value |\n\n# End\nbody",
		"[reference][id]\n\n[id]: https://example.com\n\n# Next\n[reference][id]",
		"[reference][id]\n\n# Next\n[id]: https://example.com\n",
		"[^note]: first\n  continuation\n\n  more\n\n# Heading\nbody",
		"$$\n\\sum_{i=1}^{n} i\n\n+1\n$$\n# Heading\nbody",
		"### 界 e\u0301 👩‍💻\n- **bold** [link](https://example.com) `code`\n# Next\n\x1b[31mANSI\x1b[0m\n",
		"text\n\n\n\n# Heading\n\n\nbody\n\n",
		"```go\nfmt.Println(\"x\")\n```\n# Next\n```text\nmore\n```\nend",
		"# heading\n---\n# next\nbody\r\nmore\b\n",
	}
	for i, input := range cases {
		for _, width := range []int{12, 39, 80} {
			t.Run(fmt.Sprintf("%d/%d", i, width), func(t *testing.T) {
				r := NewIncrementalRenderer(width)
				for end := 0; end <= len(input); end++ {
					got, blocks, err := r.RenderWithCodeBlocks(input[:end])
					require.NoError(t, err)
					want, wantBlocks, err := NewFastRenderer(width).RenderWithCodeBlocks(input[:end])
					require.NoError(t, err)
					require.Equal(t, want, got, "byte %d: %q", end, input[:end])
					require.Equal(t, wantBlocks, blocks, "byte %d", end)
				}
			})
		}
	}
}

func TestParserCheckpointRetainsAdjacentCompletedBlocks(t *testing.T) {
	r := NewIncrementalRenderer(80)
	var input strings.Builder
	for i := range 600 {
		chunk := fmt.Sprintf("### Item %d\n- deterministic **markdown** with `code`.\n", i)
		input.WriteString(chunk)
		_, err := r.RenderParts(input.String())
		require.NoError(t, err)
		require.LessOrEqual(t, input.Len()-len(r.inputPrefix), len(chunk), "completed blocks must not grow the mutable region")
	}
	oldPrefix := r.outputPrefix
	_, err := r.RenderParts(input.String() + "# New\nbody")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(r.outputPrefix, oldPrefix))
}

func TestParserCheckpointEditsAndWidthReset(t *testing.T) {
	r := NewIncrementalRenderer(80)
	for _, input := range []string{"# One\n- body\n# Two\ntail", "# Changed\n- body\n# Two\ntail", "", "# Restart\nbody"} {
		for _, width := range []int{80, 23, 80} {
			r.SetWidth(width)
			got, err := r.Render(input)
			require.NoError(t, err)
			want, err := NewFastRenderer(width).Render(input)
			require.NoError(t, err)
			require.Equal(t, want, got)
		}
	}
}

func FuzzParserCheckpointMatchesFullRender(f *testing.F) {
	for _, input := range []string{"# Heading\n- item\n# Next\nbody", "| a | b |\n| - | - |\n| c | d |", "```go\nx\n```\n\nbody", "[^a]: note\n\n  more\n# Next\nbody"} {
		f.Add(input, uint8(7))
	}
	f.Fuzz(func(t *testing.T, input string, step uint8) {
		if len(input) > 4096 {
			t.Skip()
		}
		r := NewIncrementalRenderer(37)
		stride := int(step)%31 + 1
		for end := 0; end < len(input)+stride; end += stride {
			prefix := input[:min(end, len(input))]
			got, blocks, err := r.RenderWithCodeBlocks(prefix)
			require.NoError(t, err)
			want, wantBlocks, err := NewFastRenderer(37).RenderWithCodeBlocks(prefix)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, wantBlocks, blocks)
		}
	})
}
