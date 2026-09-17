package styles

import (
	"fmt"
	"image/color"
	"os"
	"reflect"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

func TestBuiltinInputAndTabSurfaceHierarchy(t *testing.T) {
	t.Parallel()
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		c := theme.Colors
		assert.Equal(t, c.EditorBg, c.TabActiveBg, ref)
		assert.Equal(t, c.Background, c.TabBg, ref)
		app, ok := relativeLuminanceHex(c.Background)
		require.True(t, ok)
		input, ok := relativeLuminanceHex(c.EditorBg)
		require.True(t, ok)
		if app > 0.5 {
			assert.Less(t, input, app, ref)
		} else {
			assert.Greater(t, input, app, ref)
		}
		assert.NotEqual(t, c.TabActiveFg, c.TabInactiveFg, ref)
	}
}

func TestAppliedActiveTabAlwaysFollowsEditorSurface(t *testing.T) { //nolint:paralleltest // theme globals
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme := DefaultTheme()
	theme.Colors.EditorBg = "#123456"
	theme.Colors.TabActiveBg = "#abcdef"
	ApplyTheme(theme)
	assert.Equal(t, EditorBg, TabActiveBg)
	assert.Equal(t, "#abcdef", CurrentTheme().Colors.TabActiveBg, "legacy config value remains intact")
}

func TestPureBaseThemeVariantsCompleteAndContrasting(t *testing.T) {
	t.Parallel()
	variants := []struct{ ref, name, background string }{
		{"black-cyan", "Black Cyan", "#000000"},
		{"black-amber", "Black Amber", "#000000"},
		{"black-violet", "Black Violet", "#000000"},
		{"white-cobalt", "White Cobalt", "#FFFFFF"},
		{"white-teal", "White Teal", "#FFFFFF"},
		{"white-rose", "White Rose", "#FFFFFF"},
	}
	require.Len(t, variants, 6)
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	counts, accents := map[string]int{}, map[string]bool{}
	registeredVariants := 0
	for _, ref := range refs {
		if strings.HasPrefix(ref, "black-") || strings.HasPrefix(ref, "white-") {
			registeredVariants++
		}
	}
	require.Equal(t, 6, registeredVariants)
	for _, variant := range variants {
		require.Contains(t, refs, variant.ref)
		data, err := builtinThemes.ReadFile("themes/" + variant.ref + ".yaml")
		require.NoError(t, err)
		var raw Theme
		require.NoError(t, yaml.Unmarshal(data, &raw))
		require.Equal(t, variant.name, raw.Name)
		require.Equal(t, variant.background, raw.Colors.Background)
		require.Equal(t, variant.background, raw.Colors.ShellBg)
		require.Equal(t, variant.background, raw.Colors.TabBg)
		counts[raw.Colors.Background]++
		require.False(t, accents[raw.Colors.Accent], "variants have distinct accents")
		accents[raw.Colors.Accent] = true
		colors := reflect.ValueOf(raw.Colors)
		for _, field := range semanticColorFields {
			require.NotEmpty(t, colors.FieldByName(field).String(), "%s/%s", variant.ref, field)
		}
		require.GreaterOrEqual(t, len(raw.Colors.AgentHues), 8)
		for _, pair := range [][2]string{{raw.Colors.SelectedFg, raw.Colors.Selected}, {raw.Colors.TextPrimary, raw.Colors.CardBg}, {raw.Colors.Accent, raw.Colors.Background}} {
			ratio, ok := contrastRatioHex(pair[0], pair[1])
			require.True(t, ok)
			require.GreaterOrEqual(t, ratio, 4.5, "%s foreground/background selection and content readability", variant.ref)
		}
	}
	require.Equal(t, 3, counts["#000000"])
	require.Equal(t, 3, counts["#FFFFFF"])
}

func TestPureBaseThemeRenderedDimIdentitySelectionAndDialog(t *testing.T) {
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	for _, ref := range []string{"black-cyan", "black-amber", "black-violet", "white-cobalt", "white-teal", "white-rose"} {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		focused := BaseStyle.Background(EditorBg).Render("Send to reviewer · ready")
		fc := NewFadeContext()
		inactive := FadeLineCtx(focused, 0.62, &fc)
		require.Equal(t, ansi.Strip(focused), ansi.Strip(inactive))
		require.NotEqual(t, focused, inactive)
		fg := lipgloss.Color(theme.Colors.TextPrimary)
		r, g, b := ColorToRGB(fg)
		ir, ig, ib := fc.interpolate(r*255, g*255, b*255, 0.62)
		renderedR, renderedG, renderedB := extractFirstFgRGB(inactive)
		require.Equal(t, [3]int{ir, ig, ib}, [3]int{renderedR, renderedG, renderedB}, "ANSI rendered main foreground is the contrast-tested color")
		require.GreaterOrEqual(t, contrastRatio(RGBToColor(float64(ir)/255, float64(ig)/255, float64(ib)/255), Background), 4.5, "%s inactive body text remains readable on exact base", ref)
		// Header surfaces use the same primary text. Test the actually
		// transformed RGB against the transformed elevated surface as well.
		hr, hg, hb := ColorToRGB(CardBg)
		dr, dg, db := fc.interpolate(hr*255, hg*255, hb*255, 0.62)
		require.GreaterOrEqual(t, contrastRatio(RGBToColor(float64(ir)/255, float64(ig)/255, float64(ib)/255), RGBToColor(float64(dr)/255, float64(dg)/255, float64(db)/255)), 4.5, "%s dimmed primary header text retains contrast on its dimmed surface", ref)
		modelStyle := BaseStyle
		require.GreaterOrEqual(t, contrastRatio(modelStyle.GetForeground(), Background), 3.0)
		require.NotEmpty(t, AgentIdentityStyle("reviewer", false).Render("reviewer"))
		selection := SelectionStyle.Render("selected λ界")
		require.GreaterOrEqual(t, contrastRatio(SelectionStyle.GetForeground(), SelectionStyle.GetBackground()), 7.0, "%s actual applied selection style must remain readable", ref)
		sr, sg, sb := extractFirstFgRGB(selection)
		require.Equal(t, strings.ToLower(theme.Colors.TextPrimary), RGBToHex(float64(sr)/255, float64(sg)/255, float64(sb)/255), "selection uses effective primary foreground, not an unreachable YAML promise")
		require.Equal(t, "selected λ界", ansi.Strip(selection))
		t.Logf("%s focused=%q inactive=%q model=%q selection=%q", ref, focused, inactive, modelStyle.Render("Friendly Model provider (high)"), selection)
	}
}
