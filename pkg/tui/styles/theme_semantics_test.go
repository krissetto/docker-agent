package styles

import (
	"fmt"
	"image/color"
	"os"
	"reflect"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var semanticColorFields = []string{
	"EditorBg", "CardBg", "ShellBg",
	"TabActiveBg", "TabActiveFg", "TabInactiveFg", "TabHoverBg", "TabHoverFg", "TabDragBg", "TabDragFg", "TabBusy",
	"ContextEmpty", "ContextFill", "ContextText",
	"Resize", "ResizeHover", "ResizeActive", "Separator",
	"Placeholder", "Hint", "Focus", "Disabled", "StatusAdjacent",
}

func TestBuiltinThemesExplicitlyDefineSemanticColors(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		data, err := builtinThemes.ReadFile("themes/" + ref + ".yaml")
		require.NoError(t, err)
		var raw Theme
		require.NoError(t, yaml.Unmarshal(data, &raw))
		colors := reflect.ValueOf(raw.Colors)
		for _, field := range semanticColorFields {
			assert.NotEmpty(t, colors.FieldByName(field).String(), "%s must explicitly define %s", ref, field)
		}
	}
}

type semanticContrast struct {
	fg, bg string
	min    float64
	desc   string
}

// Text and meaningful icons follow WCAG AA's 4.5:1 normal-text target where
// practical. State icons use WCAG's 3:1 non-text target. Subtle/decorative
// resize, separator, disabled, and empty-track cells are the narrow exceptions
// at 1.5:1. Surface separation is deterministic at 1.08:1.
var semanticContrasts = []semanticContrast{
	{"TextPrimary", "EditorBg", 4.5, "editor text"},
	{"Placeholder", "EditorBg", 3.0, "input placeholder"},
	{"Hint", "EditorBg", 3.0, "input hint"},
	{"Focus", "EditorBg", 3.0, "focus/cursor"},
	{"TabActiveFg", "TabActiveBg", 4.5, "active tab text"},
	{"TabInactiveFg", "ShellBg", 3.0, "inactive tab text"},
	{"TabHoverFg", "TabHoverBg", 4.5, "hover tab text"},
	{"TabDragFg", "TabDragBg", 4.5, "drag tab text"},
	{"TabBusy", "ShellBg", 3.0, "busy spinner"},
	{"ContextText", "ShellBg", 3.0, "context label"},
	{"ContextFill", "EditorBg", 3.0, "context fill"},
	{"StatusAdjacent", "ShellBg", 3.0, "adjacent status"},
	{"Resize", "ShellBg", 1.5, "decorative resize"},
	{"ResizeHover", "ShellBg", 3.0, "hover resize"},
	{"ResizeActive", "ShellBg", 3.0, "active resize"},
	{"Separator", "CardBg", 1.5, "decorative separator"},
	{"Disabled", "ShellBg", 1.5, "disabled decoration"},
	{"EditorBg", "ShellBg", 1.08, "input/page surface distinction"},
	{"CardBg", "ShellBg", 1.08, "card/page surface distinction"},
}

func TestBuiltinThemeSemanticContrast(t *testing.T) {
	t.Parallel()

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		colors := reflect.ValueOf(theme.Colors)
		for _, check := range semanticContrasts {
			fg := colors.FieldByName(check.fg).String()
			bg := colors.FieldByName(check.bg).String()
			ratio, ok := contrastRatioHex(fg, bg)
			require.True(t, ok, "%s %s has invalid colors %q/%q", ref, check.desc, fg, bg)
			assert.GreaterOrEqualf(t, ratio, check.min, "%s %s: %s on %s = %.2f:1; need %.2f:1", ref, check.desc, fg, bg, ratio, check.min)
		}
	}
}

func TestApplyThemeWiresSemanticColors(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	theme := DefaultTheme()
	ApplyTheme(theme)
	wired := map[string]struct {
		got  color.Color
		want string
	}{
		"editor": {EditorBg, theme.Colors.EditorBg}, "card": {CardBg, theme.Colors.CardBg}, "shell": {ShellBg, theme.Colors.ShellBg},
		"tab hover bg": {TabHoverBg, theme.Colors.TabHoverBg}, "tab hover fg": {TabHoverFg, theme.Colors.TabHoverFg},
		"tab drag bg": {TabDragBg, theme.Colors.TabDragBg}, "tab drag fg": {TabDragFg, theme.Colors.TabDragFg}, "tab busy": {TabBusy, theme.Colors.TabBusy},
		"context empty": {ContextEmpty, theme.Colors.ContextEmpty}, "context fill": {ContextFill, theme.Colors.ContextFill}, "context text": {ContextText, theme.Colors.ContextText},
		"resize": {Resize, theme.Colors.Resize}, "resize hover": {ResizeHover, theme.Colors.ResizeHover}, "resize active": {ResizeActive, theme.Colors.ResizeActive},
		"hint": {Hint, theme.Colors.Hint}, "focus": {Focus, theme.Colors.Focus}, "disabled": {Disabled, theme.Colors.Disabled}, "status adjacent": {StatusAdjacent, theme.Colors.StatusAdjacent},
	}
	for name, check := range wired {
		assert.Equal(t, strings.ToLower(check.want), RGBToHex(ColorToRGB(check.got)), name)
	}
	assert.Equal(t, EditorBg, EditorStyle.GetBackground(), "editor surface uses semantic editor token")
	assert.Equal(t, EditorBg, SuggestionGhostStyle.GetBackground(), "suggestion surface follows editor token")
	assert.Equal(t, Resize, ResizeHandleStyle.GetForeground())
	assert.Equal(t, ResizeHover, ResizeHandleHoverStyle.GetForeground())
	assert.Equal(t, ResizeActive, ResizeHandleActiveStyle.GetForeground())
}

func TestBuiltinThemeSemanticANSIMatrix(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	var snapshot strings.Builder
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		light := "dark"
		if luminance, ok := relativeLuminanceHex(theme.Colors.Background); ok && luminance > 0.5 {
			light = "light"
		}
		cells := []string{
			lipgloss.NewStyle().Foreground(TextPrimary).Background(EditorBg).Render("focus"),
			lipgloss.NewStyle().Foreground(Hint).Background(EditorBg).Render("blur"),
			lipgloss.NewStyle().Foreground(ContextEmpty).Background(EditorBg).Render("ctx0"),
			lipgloss.NewStyle().Foreground(ContextFill).Background(EditorBg).Render("ctx50"),
			lipgloss.NewStyle().Foreground(Error).Background(EditorBg).Render("ctx95"),
			lipgloss.NewStyle().Foreground(TabActiveFg).Background(TabActiveBg).Render("active"),
			lipgloss.NewStyle().Foreground(TabInactiveFg).Background(ShellBg).Render("idle"),
			lipgloss.NewStyle().Foreground(TabBusy).Background(ShellBg).Render("busy"),
		}
		fmt.Fprintf(&snapshot, "%s %s %q\n", ref, light, strings.Join(cells, " "))
	}

	const golden = "testdata/theme_semantic_matrix.golden"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(golden, []byte(snapshot.String()), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "run UPDATE_GOLDEN=1 go test ./pkg/tui/styles -run SemanticANSIMatrix")
	assert.Equal(t, string(want), snapshot.String())
}
