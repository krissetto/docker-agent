package markdown

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestIncrementalWarmPrefixTracksThemeGeneration(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	r := NewIncrementalRenderer(60)
	const input = "# Stable heading\n\n```go\nvar value = 1\n```\n\nmutable tail"
	before, err := r.Render(input)
	require.NoError(t, err)
	require.NotEmpty(t, r.outputPrefix)
	theme := *original
	theme.Colors.Background, theme.Colors.TextPrimary = "#ffffff", "#111111"
	theme.Markdown.Heading = "#112233"
	styles.ApplyTheme(&theme)
	got, blocks, err := r.RenderWithCodeBlocks(input)
	require.NoError(t, err)
	fresh, freshBlocks, err := NewIncrementalRenderer(60).RenderWithCodeBlocks(input)
	require.NoError(t, err)
	require.Equal(t, fresh, got)
	require.Equal(t, freshBlocks, blocks)
	require.NotEqual(t, before, got)
}
