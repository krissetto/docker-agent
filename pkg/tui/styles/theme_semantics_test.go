package styles

import (
	"fmt"
	"image/color"
	"os"
	"reflect"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
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
	"Placeholder", "Hint", "Focus", "Disabled", "StatusAdjacent", "ScrollbarTrack",
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
	assert.Equal(t, Separator, BorderMuted, "muted separators use the authored semantic separator role")
	assert.Equal(t, BorderMuted, DialogSeparatorStyle.GetForeground())
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
			TrackStyle.Render("track"),
			ThumbStyle.Render("thumb"),
			ThumbActiveStyle.Render("drag"),
			ToolErrorIcon.Render("err"),
			ToolPendingIcon.Render("wait"),
			DiffAddEmphStyle.Render("add!"),
			DiffRemoveEmphStyle.Render("del!"),
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
		for _, pair := range [][2]string{{raw.Colors.TextPrimary, raw.Colors.Selected}, {raw.Colors.TextPrimary, raw.Colors.CardBg}, {raw.Colors.Accent, raw.Colors.Background}} {
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

func TestBlackThemeMenuAndSpinnerContrast(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	for _, ref := range []string{"black-amber", "black-cyan", "black-violet"} {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		for name, style := range map[string]lipgloss.Style{
			"palette action":         PaletteSelectedActionStyle,
			"palette description":    PaletteSelectedDescStyle,
			"completion label":       CompletionSelectedStyle,
			"completion description": CompletionSelectedDescStyle,
		} {
			assert.Equal(t, MobyBlue, style.GetBackground(), "%s %s", ref, name)
			assert.GreaterOrEqual(t, contrastRatio(style.GetForeground(), style.GetBackground()), 7.0, "%s %s", ref, name)
			r, g, b := extractFirstFgRGB(style.Render("selected"))
			assert.Equal(t, strings.ToLower(theme.Colors.SelectedFg), RGBToHex(float64(r)/255, float64(g)/255, float64(b)/255), "%s %s rendered foreground", ref, name)
		}
		assert.Equal(t, TextPrimary, SelectionStyle.GetForeground(), "dark selection retains light text")
		assert.GreaterOrEqual(t, contrastRatio(SelectionStyle.GetForeground(), SelectionStyle.GetBackground()), 7.0, ref)

		// The sweep must brighten the resting accent without a dark halo or a white flash.
		previous := relativeLuminanceColor(Accent)
		accentHue, _, _ := rgbToHSL(ColorToRGB(Accent))
		for _, step := range []struct {
			style lipgloss.Style
			want  string
		}{
			{SpinnerTextDimmestStyle, theme.Colors.Accent},
			{SpinnerTextDimStyle, theme.Colors.SpinnerDim},
			{SpinnerTextBrightStyle, theme.Colors.SpinnerBright},
			{SpinnerTextBrightestStyle, theme.Colors.SpinnerBrightest},
		} {
			fg := step.style.GetForeground()
			assert.Equal(t, strings.ToLower(step.want), RGBToHex(ColorToRGB(fg)), ref)
			assert.GreaterOrEqual(t, contrastRatio(fg, ShellBg), 4.5, "%s spinner on page", ref)
			assert.GreaterOrEqual(t, contrastRatio(fg, EditorBg), 4.5, "%s spinner on editor", ref)
			luminance := relativeLuminanceColor(fg)
			assert.GreaterOrEqual(t, luminance, previous, "%s spinner ramp only brightens", ref)
			previous = luminance
			r, g, b := ColorToRGB(fg)
			hue, saturation, _ := rgbToHSL(r, g, b)
			assert.InDelta(t, accentHue, hue, 5.0, "%s spinner keeps its accent hue", ref)
			assert.Greater(t, saturation, 0.5, "%s spinner remains saturated", ref)
		}
	}
}

func TestAllBuiltinTextAndControlContrast(t *testing.T) {
	t.Parallel()
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		c := theme.Colors
		checks := []semanticContrast{
			{"TextPrimary", "Background", 4.5, "body text"},
			{"TextPrimary", "BackgroundAlt", 4.5, "card text"},
			{"TextPrimary", "EditorBg", 4.5, "editor text"},
			{"TextPrimary", "CardBg", 4.5, "card text"},
			{"TextSecondary", "ShellBg", 4.5, "secondary text"},
			{"TextSecondary", "CardBg", 4.5, "secondary card text"},
			{"TextMuted", "ShellBg", 4.5, "muted text"},
			{"TextMuted", "CardBg", 4.5, "muted card text"},
			{"Placeholder", "EditorBg", 4.5, "placeholder text"},
			{"Hint", "EditorBg", 4.5, "input hint"},
			{"SuggestionGhost", "EditorBg", 4.5, "suggestion text"},
			{"ContextText", "ShellBg", 4.5, "context text"},
			{"StatusAdjacent", "ShellBg", 4.5, "adjacent status text"},
			{"LineNumber", "BackgroundAlt", 4.5, "line number text"},
			{"TabActiveFg", "TabActiveBg", 4.5, "active tab text"},
			{"TabInactiveFg", "ShellBg", 4.5, "inactive tab text"},
			{"TabHoverFg", "TabHoverBg", 4.5, "hover tab text"},
			{"TabDragFg", "TabDragBg", 4.5, "drag tab text"},
			{"TabPrimaryFg", "ShellBg", 4.5, "tab title text"},
			{"Accent", "Background", 4.5, "accent text"},
			{"Accent", "EditorBg", 3.0, "editor accent text"},
			{"Accent", "CardBg", 3.0, "card accent text"},
			{"Success", "Background", 4.5, "success text"},
			{"Success", "EditorBg", 4.5, "editor success text"},
			{"Success", "CardBg", 4.5, "card success text"},
			{"Error", "Background", 4.5, "error text"},
			{"Error", "EditorBg", 4.5, "editor error text"},
			{"Error", "CardBg", 4.5, "card error text"},
			{"Warning", "Background", 4.5, "warning text"},
			{"Warning", "EditorBg", 4.5, "editor warning text"},
			{"Warning", "CardBg", 4.5, "card warning text"},
			{"Info", "Background", 4.5, "info text"},
			{"Info", "EditorBg", 4.5, "editor info text"},
			{"Info", "CardBg", 4.5, "card info text"},
			{"Highlight", "Background", 4.5, "highlight text"},
			{"Highlight", "EditorBg", 4.5, "editor highlight text"},
			{"Highlight", "CardBg", 4.5, "card highlight text"},
			{"TextPrimary", "Selected", 4.5, "selected text"},
			{"Success", "DiffAddBg", 4.5, "addition text"},
			{"Error", "DiffRemoveBg", 4.5, "removal text"},
			{"BrandText", "Brand", 4.5, "selected command text"},
		}
		colors := reflect.ValueOf(c)
		for _, check := range checks {
			var fg string
			switch check.fg {
			case "BrandText":
				fg = c.SelectedFg
			case "TabPrimaryFg":
				fg = c.TextMuted
			default:
				fg = colors.FieldByName(check.fg).String()
			}
			bg := colors.FieldByName(check.bg).String()
			ratio, ok := contrastRatioHex(fg, bg)
			require.True(t, ok, "%s %s has invalid colors %q/%q", ref, check.desc, fg, bg)
			assert.GreaterOrEqualf(t, ratio, check.min, "%s %s: %s on %s = %.2f:1", ref, check.desc, fg, bg, ratio)
		}

		codeBg := theme.Markdown.CodeBg
		for name, fg := range map[string]string{
			"syntax comment": theme.Chroma.Comment, "syntax preprocessor": theme.Chroma.CommentPreproc,
			"syntax keyword": theme.Chroma.Keyword, "syntax reserved": theme.Chroma.KeywordReserved,
			"syntax namespace": theme.Chroma.KeywordNamespace, "syntax type": theme.Chroma.KeywordType,
			"syntax operator": theme.Chroma.Operator, "syntax punctuation": theme.Chroma.Punctuation,
			"syntax builtin": theme.Chroma.NameBuiltin, "syntax tag": theme.Chroma.NameTag,
			"syntax attribute": theme.Chroma.NameAttribute, "syntax decorator": theme.Chroma.NameDecorator,
			"syntax number": theme.Chroma.LiteralNumber, "syntax string": theme.Chroma.LiteralString,
			"syntax escape": theme.Chroma.LiteralStringEscape, "syntax deleted": theme.Chroma.GenericDeleted,
			"syntax subheading": theme.Chroma.GenericSubheading, "syntax success": theme.Chroma.Success,
			"inline code": theme.Markdown.Code,
		} {
			ratio, ok := contrastRatioHex(fg, codeBg)
			require.True(t, ok, "%s %s has invalid colors %q/%q", ref, name, fg, codeBg)
			assert.GreaterOrEqualf(t, ratio, 4.5, "%s %s: %s on code background %s = %.2f:1", ref, name, fg, codeBg, ratio)
		}
		if theme.Chroma.ErrorFg != "" && theme.Chroma.ErrorBg != "" {
			ratio, ok := contrastRatioHex(theme.Chroma.ErrorFg, theme.Chroma.ErrorBg)
			require.True(t, ok)
			assert.GreaterOrEqualf(t, ratio, 4.5, "%s syntax error fg on background = %.2f:1", ref, ratio)
		}
		for _, fg := range []string{theme.Markdown.Heading, theme.Markdown.Link, theme.Markdown.Strong, theme.Markdown.Blockquote, theme.Markdown.List} {
			ratio, ok := contrastRatioHex(fg, c.Background)
			require.True(t, ok)
			assert.GreaterOrEqualf(t, ratio, 4.5, "%s markdown %s on document background = %.2f:1", ref, fg, ratio)
		}
	}
}

func TestAllBuiltinScrollbarAndDiffStylesRenderWithIntendedRoles(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)

		assert.True(t, isNoColor(TrackStyle.GetBackground()), "%s track must not paint the gutter", ref)
		assert.True(t, isNoColor(ThumbStyle.GetBackground()), "%s thumb must leave neighboring panel background intact", ref)
		assert.True(t, isNoColor(ThumbActiveStyle.GetBackground()), "%s active thumb must leave neighboring panel background intact", ref)
		track := TrackStyle.Render("│")
		tr, tg, tb := extractFirstFgRGB(track)
		assert.Equal(t, RGBToHex(ColorToRGB(ScrollbarTrack)), RGBToHex(float64(tr)/255, float64(tg)/255, float64(tb)/255), "%s exact track ANSI foreground", ref)
		cursor := SuggestionCursorStyle.Render("λ")
		assert.Equal(t, "λ", ansi.Strip(cursor), "%s cursor suggestion preserves glyph", ref)
		cr, cg, cb := extractFirstFgRGB(cursor)
		assert.GreaterOrEqual(t, contrastRatio(RGBToColor(float64(cr)/255, float64(cg)/255, float64(cb)/255), Accent), 4.5, "%s first ghost glyph on accent cursor background", ref)
		for _, surface := range []color.Color{ShellBg, CardBg, EditorBg} {
			assert.GreaterOrEqual(t, contrastRatio(ThumbStyle.GetForeground(), surface), 3.0, "%s thumb on actual surface", ref)
			assert.GreaterOrEqual(t, contrastRatio(ThumbActiveStyle.GetForeground(), surface), 3.0, "%s active thumb on actual surface", ref)
		}
		assert.GreaterOrEqual(t, contrastRatio(White, MobyBlue), 4.5, "%s command/completion selection text", ref)
		for _, check := range []struct {
			name  string
			style lipgloss.Style
			bg    color.Color
		}{
			{"error icon", ToolErrorIcon, ErrorStrong}, {"pending icon", ToolPendingIcon, Warning},
		} {
			r, g, b := extractFirstFgRGB(check.style.Render("!"))
			assert.GreaterOrEqual(t, contrastRatio(RGBToColor(float64(r)/255, float64(g)/255, float64(b)/255), check.bg), 3.0, "%s %s glyph", ref, check.name)
		}

		for _, check := range []struct {
			name  string
			style lipgloss.Style
			row   color.Color
		}{
			{"addition", DiffAddEmphStyle, DiffAddBg}, {"removal", DiffRemoveEmphStyle, DiffRemoveBg},
		} {
			assert.NotEqual(t, check.row, check.style.GetBackground(), "%s %s emphasis remains distinct", ref, check.name)
			assert.GreaterOrEqual(t, contrastRatio(check.style.GetForeground(), check.style.GetBackground()), 4.5, "%s %s emphasized text", ref, check.name)
		}
	}
}

func TestPartialThemeScrollbarTrackFallbackAndExplicitOverride(t *testing.T) {
	t.Parallel()
	base := DefaultTheme()
	custom := mergeTheme(base, &Theme{Colors: ThemeColors{Background: "#ffffff"}})
	assert.NotEqual(t, base.Colors.ScrollbarTrack, custom.Colors.ScrollbarTrack)
	assert.Equal(t, custom.Colors.ScrollbarTrack, deriveScrollbarTrack(custom.Colors.ShellBg, custom.Colors.CardBg, custom.Colors.EditorBg))
	indexed := mergeTheme(base, &Theme{Colors: ThemeColors{Background: "4"}})
	assert.NotEqual(t, "4", indexed.Colors.ScrollbarTrack)
	explicit := mergeTheme(base, &Theme{Colors: ThemeColors{ScrollbarTrack: "39"}})
	assert.Equal(t, "39", explicit.Colors.ScrollbarTrack, "explicit ANSI scrollbar role is preserved")
}

func isNoColor(c color.Color) bool {
	_, ok := c.(lipgloss.NoColor)
	return ok
}

func TestDirectThemeScrollbarFallbackKeepsDefaultCopyIndependent(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	base := DefaultTheme()
	want := base.Colors.ScrollbarTrack
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })

	custom := *base
	custom.Colors.ShellBg = "#ffffff"
	custom.Colors.CardBg = "#f0f0f0"
	custom.Colors.EditorBg = "#eeeeee"
	custom.Colors.ScrollbarTrack = ""
	ApplyTheme(&custom)
	assert.Equal(t, deriveScrollbarTrack(custom.Colors.ShellBg, custom.Colors.CardBg, custom.Colors.EditorBg), RGBToHex(ColorToRGB(ScrollbarTrack)))
	assert.Equal(t, want, DefaultTheme().Colors.ScrollbarTrack, "mutating a returned default theme copy never changes the cached default")
}

func TestCustomSuggestionCursorUsesContrastForegroundWithoutRecoloringGhost(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme := DefaultTheme()
	theme.Colors.Accent = "#1e66f5"
	theme.Colors.SuggestionGhost = "#a0a0a0"
	ApplyTheme(theme)
	r, g, b := extractFirstFgRGB(SuggestionCursorStyle.Render("a"))
	cursorForeground := RGBToColor(float64(r)/255, float64(g)/255, float64(b)/255)
	assert.GreaterOrEqual(t, contrastRatio(cursorForeground, Accent), 4.5)
	assert.Equal(t, theme.Colors.SuggestionGhost, RGBToHex(ColorToRGB(SuggestionGhostStyle.GetForeground())), "ordinary ghost text keeps the custom theme color")
}

func TestAllBuiltinChromaStyleCompositions(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	refs, err := listBuiltinThemeRefs()
	require.NoError(t, err)
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		style := ChromaStyle()
		for name, token := range map[string]chroma.TokenType{
			"NameClass": chroma.NameClass, "NameFunction": chroma.NameFunction, "GenericInserted": chroma.GenericInserted,
		} {
			entry := style.Get(token)
			fg := entry.Colour.String()
			bg := theme.Markdown.CodeBg
			ratio, ok := contrastRatioHex(fg, bg)
			require.True(t, ok, "%s %s token style resolves to foreground/background %q/%q", ref, name, fg, bg)
			assert.GreaterOrEqualf(t, ratio, 4.5, "%s %s token style %s on %s = %.2f:1", ref, name, fg, bg, ratio)
		}
	}
}

func TestANSIStatusIconForegroundsKeepContrast(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	theme := DefaultTheme()
	theme.Colors.ErrorStrong = "0"
	theme.Colors.Warning = "0"
	ApplyTheme(theme)
	for _, check := range []struct {
		style lipgloss.Style
		bg    color.Color
	}{{ToolErrorIcon, ErrorStrong}, {ToolPendingIcon, Warning}} {
		r, g, b := extractFirstFgRGB(check.style.Render("!"))
		assert.GreaterOrEqual(t, contrastRatio(RGBToColor(float64(r)/255, float64(g)/255, float64(b)/255), check.bg), 3.0)
	}
}

func TestBuiltinSpinnerRampStartsAtAccentAndOnlyBrightens(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates globals.
	original := CurrentTheme()
	t.Cleanup(func() { ApplyTheme(original) })
	refs := []string{"solarized-dark", "nord", "gruvbox-dark"}
	for _, ref := range refs {
		theme, err := loadBuiltinTheme(ref)
		require.NoError(t, err)
		ApplyTheme(theme)
		steps := []struct {
			style lipgloss.Style
			want  string
		}{
			{SpinnerTextDimmestStyle, theme.Colors.Accent},
			{SpinnerTextDimStyle, theme.Colors.SpinnerDim},
			{SpinnerTextBrightStyle, theme.Colors.SpinnerBright},
			{SpinnerTextBrightestStyle, theme.Colors.SpinnerBrightest},
		}
		previous := relativeLuminanceColor(Accent)
		lastColor := ""
		for index, step := range steps {
			assert.Equal(t, strings.ToLower(step.want), RGBToHex(ColorToRGB(step.style.GetForeground())), "%s spinner foreground", ref)
			colorHex := RGBToHex(ColorToRGB(step.style.GetForeground()))
			if index > 1 {
				assert.NotEqualf(t, lastColor, colorHex, "%s spinner ramp has visible steps", ref)
			}
			luminance := relativeLuminanceColor(step.style.GetForeground())
			assert.GreaterOrEqualf(t, luminance, previous, "%s spinner ramp does not dim after accent", ref)
			previous, lastColor = luminance, colorHex
		}
	}
}
