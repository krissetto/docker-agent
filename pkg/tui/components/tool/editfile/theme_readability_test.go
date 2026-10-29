package editfile

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/aymanbagabas/go-udiff"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestBuiltinThemesRenderReadableChromaErrorTokensInContext(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		if !styles.IsBuiltinTheme(ref) {
			continue
		}
		t.Run(ref, func(t *testing.T) {
			theme, err := styles.LoadTheme(ref)
			require.NoError(t, err)
			styles.ApplyTheme(theme)

			chromaStyle := styles.ChromaStyle()
			entry := chromaStyle.Get(chroma.Error)
			require.True(t, entry.Colour.IsSet(), "error token needs an authored foreground")
			require.True(t, entry.Background.IsSet(), "error token needs an authored background")
			errorStyle := chromaToLipgloss(chroma.Error, chromaStyle)
			require.Equal(t, lipgloss.Color(entry.Colour.String()), errorStyle.GetForeground(), "rendered error foreground")
			require.Equal(t, lipgloss.Color(entry.Background.String()), errorStyle.GetBackground(), "rendered error background")

			// Python's invalid '$' produces a real Chroma.Error token surrounded
			// by ordinary syntax, exercising the same unchanged-row path as context.
			diff := []*udiff.Hunk{{
				FromLine: 1,
				ToLine:   1,
				Lines:    []udiff.Line{{Kind: udiff.Equal, Content: "broken = $"}},
			}}
			for name, rendered := range map[string]string{
				"unified": renderDiffWithSyntaxHighlight(diff, "main.py", 80),
				"split":   renderSplitDiffWithSyntaxHighlight(diff, "main.py", 120),
			} {
				t.Run(name, func(t *testing.T) {
					require.Contains(t, rendered, errorStyle.Render("$"), "error token retains its authored fg/bg in context")
					nameTokens := syntaxHighlight("broken", "main.py")
					require.NotEmpty(t, nameTokens)
					nameStyle := nameTokens[0].Style.Background(styles.DiffUnchangedStyle.GetBackground())
					require.Contains(t, rendered, nameStyle.Render("broken"), "ordinary syntax keeps its own style, not ErrorBg")
				})
			}
		})
	}
}

func TestChromaErrorTokenDefersBackgroundAndForegroundToDiffRows(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates package-wide styles.
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())

	chromaStyle := styles.ChromaStyle()
	errorStyle := chromaToLipgloss(chroma.Error, chromaStyle)
	normalStyle := chromaToLipgloss(chroma.Name, chromaStyle)
	require.True(t, isNoColor(normalStyle.GetBackground()), "ordinary token has no explicit chroma background")

	for _, tc := range []struct {
		name string
		kind udiff.OpKind
		line lipgloss.Style
	}{
		{name: "remove", kind: udiff.Delete, line: styles.DiffRemoveStyle},
		{name: "add", kind: udiff.Insert, line: styles.DiffAddStyle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens := []chromaToken{{Text: "$", Style: errorStyle}}
			actual := renderTokensWithStyle(tokens, tc.line, tc.kind)
			want := errorStyle.Foreground(tc.line.GetForeground()).Background(tc.line.GetBackground()).Render("$")
			require.Equal(t, want, actual, "diff foreground/background take priority for error tokens")

			tokens[0].Emphasized = true
			actual = renderTokensWithStyle(tokens, tc.line, tc.kind)
			want = errorStyle.Foreground(tc.line.GetForeground()).Background(emphasisStyleFor(tc.kind).GetBackground()).Bold(true).Render("$")
			require.Equal(t, want, actual, "word emphasis takes background priority while preserving diff foreground")

			ordinary := renderTokensWithStyle([]chromaToken{{Text: "name", Style: normalStyle}}, tc.line, tc.kind)
			want = normalStyle.Background(tc.line.GetBackground()).Render("name")
			require.Equal(t, want, ordinary, "ordinary syntax foreground remains intact on diff rows")
		})
	}
}
