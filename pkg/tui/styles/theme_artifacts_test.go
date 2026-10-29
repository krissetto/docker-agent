package styles

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

var sgrSequence = regexp.MustCompile("\x1b\\[([0-9;]*)m")

// emittedTrueColors returns the first truecolor foreground and background in s.
func emittedTrueColors(s string) (fg, bg string) {
	for _, m := range sgrSequence.FindAllStringSubmatch(s, -1) {
		parts := strings.Split(m[1], ";")
		for i := 0; i+4 < len(parts); i++ {
			if parts[i+1] != "2" || (parts[i] != "38" && parts[i] != "48") {
				continue
			}
			var c [3]int
			for k := range 3 {
				c[k], _ = strconv.Atoi(parts[i+2+k])
			}
			hex := fmt.Sprintf("#%02X%02X%02X", c[0], c[1], c[2])
			if parts[i] == "38" && fg == "" {
				fg = hex
			}
			if parts[i] == "48" && bg == "" {
				bg = hex
			}
			i += 4
		}
	}
	return fg, bg
}

// Set DOCKER_NEXT_ARTIFACT_DIR to write rendered ANSI cases and an emitted-SGR contrast table.
func TestThemeRenderedArtifacts(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	dir := os.Getenv("DOCKER_NEXT_ARTIFACT_DIR")
	if dir == "" {
		t.Skip("DOCKER_NEXT_ARTIFACT_DIR not set")
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))

	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	type artifactCase struct {
		name  string
		style lipgloss.Style
		min   float64
	}
	var table strings.Builder
	table.WriteString("theme\tcase\tfg\tbg\tratio\tminimum\tstatus\tbg_source\n")

	for _, ref := range []string{DefaultThemeRef, DockerNextLightThemeRef, LegacyDefaultThemeRef} {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		md := theme.Markdown
		cases := []artifactCase{
			{"tab-active", lipgloss.NewStyle().Foreground(TabActiveFg).Background(TabActiveBg), 4.5},
			{"tab-inactive", lipgloss.NewStyle().Foreground(TabInactiveFg).Background(TabBg), 4.5},
			{"tab-hover", lipgloss.NewStyle().Foreground(TabHoverFg).Background(TabHoverBg), 4.5},
			{"tab-drag", lipgloss.NewStyle().Foreground(TabDragFg).Background(TabDragBg), 4.5},
			{"tab-busy", lipgloss.NewStyle().Foreground(TabBusy).Background(TabBg), 4.5},
			{"editor-text", lipgloss.NewStyle().Foreground(TextPrimary).Background(EditorBg), 4.5},
			{"editor-cursor", SuggestionCursorStyle, 4.5},
			{"editor-ghost", lipgloss.NewStyle().Foreground(SuggestionGhostStyle.GetForeground()).Background(EditorBg), 4.5},
			{"placeholder", lipgloss.NewStyle().Foreground(PlaceholderColor).Background(EditorBg), 4.5},
			{"context-empty", lipgloss.NewStyle().Foreground(ContextEmpty).Background(EditorBg), 3},
			{"context-fill", lipgloss.NewStyle().Foreground(ContextFill).Background(EditorBg), 3},
			{"context-warn", lipgloss.NewStyle().Foreground(Error).Background(EditorBg), 3},
			{"md-heading", lipgloss.NewStyle().Foreground(lipgloss.Color(md.Heading)).Background(Background), 4.5},
			{"md-link", lipgloss.NewStyle().Foreground(lipgloss.Color(md.Link)).Background(Background), 4.5},
			{"md-strong", lipgloss.NewStyle().Foreground(lipgloss.Color(md.Strong)).Background(Background), 4.5},
			{"md-inline-code", lipgloss.NewStyle().Foreground(lipgloss.Color(md.Code)).Background(lipgloss.Color(md.CodeBg)), 4.5},
			{"md-quote", lipgloss.NewStyle().Foreground(lipgloss.Color(md.Blockquote)).Background(Background), 4.5},
			{"user-message", lipgloss.NewStyle().Foreground(TextPrimary).Background(BackgroundAlt), 4.5},
			{"tool-completed-icon", ToolCompletedIcon, 3},
			{"tool-error-icon", ToolErrorIcon, 3},
			{"tool-pending-icon", ToolPendingIcon, 3},
			{"diff-add", DiffAddStyle, 4.5},
			{"diff-add-emph", DiffAddEmphStyle, 4.5},
			{"diff-remove", DiffRemoveStyle, 4.5},
			{"diff-remove-emph", DiffRemoveEmphStyle, 4.5},
			{"todo-in-progress", InProgressStyle, 4.5},
			{"agent-badge", AgentBadgeStyle, 4.5},
			{"scroll-thumb", ThumbStyle, 3},
			{"scroll-thumb-active", ThumbActiveStyle, 3},
			{"selection", SelectionStyle, 4.5},
			{"palette-selected", PaletteSelectedActionStyle, 4.5},
			{"completion-selected", CompletionSelectedStyle, 4.5},
			{"dialog-title-warning", DialogTitleWarningStyle, 4.5},
			{"dialog-help", DialogHelpStyle, 4.5},
			{"dialog-highlight", lipgloss.NewStyle().Foreground(Highlight).Background(Background), 4.5},
			{"dialog-border", lipgloss.NewStyle().Foreground(BorderSecondary).Background(Background), 3},
		}

		var ansi strings.Builder
		for _, c := range cases {
			rendered := c.style.Render("Aa")
			fgHex, bgHex := emittedTrueColors(rendered)
			source := "emitted"
			if bgHex == "" {
				bgHex = theme.Colors.Background
				source = "assumed-page-background"
			}
			if fgHex == "" {
				fgHex = theme.Colors.TextPrimary
				source = "assumed-fg"
			}
			ratio, ok := contrastRatioHex(fgHex, bgHex)
			require.True(t, ok, "%s %s", ref, c.name)
			status := "PASS"
			if ratio < c.min {
				status = "FLAG"
			}
			fmt.Fprintf(&table, "%s\t%s\t%s\t%s\t%.2f\t%.1f\t%s\t%s\n", ref, c.name, fgHex, bgHex, ratio, c.min, status, source)
			fmt.Fprintf(&ansi, "%s\t%s\n", c.name, rendered)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, ref+".ans"), []byte(ansi.String()), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contrast.tsv"), []byte(table.String()), 0o644))
}
