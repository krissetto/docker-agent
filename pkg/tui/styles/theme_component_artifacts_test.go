package styles_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Gated by DOCKER_NEXT_COMPONENT_DIR; records ratios instead of failing so
// flagged pairs reach the designer as evidence.
const componentSampleMarkdown = "# Heading\n\nA [link](https://example.com) and **strong** with `inline code`.\n\n> quoted line\n\n- item one\n- item two\n\n---\n\n```go\nfunc main() { fmt.Println(\"hi\") }\n```\n\n```yaml\nkey: value # comment\n```\n"

const componentTextMin = 4.5

var (
	sgrPrefix = regexp.MustCompile(`^\x1b\[([0-9;]*)m`)
	csiOther  = regexp.MustCompile(`^\x1b\[[0-9;?]*[A-Za-z@]`)
	oscSeq    = regexp.MustCompile(`^\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
)

type rgbValue struct{ r, g, b int }

type cellRecord struct {
	fg, bg rgbValue
	bgSet  bool
	ch     rune
}

type sgrState struct {
	fg, bg                rgbValue
	fgSet, bgSet, reverse bool
}

func (st *sgrState) apply(params string) {
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "", "0":
			*st = sgrState{}
		case "7":
			st.reverse = true
		case "27":
			st.reverse = false
		case "39":
			st.fgSet = false
		case "49":
			st.bgSet = false
		case "38", "48":
			if i+4 < len(parts) && parts[i+1] == "2" {
				c := rgbValue{atoiSafe(parts[i+2]), atoiSafe(parts[i+3]), atoiSafe(parts[i+4])}
				if parts[i] == "38" {
					st.fg, st.fgSet = c, true
				} else {
					st.bg, st.bgSet = c, true
				}
				i += 4
			}
		}
	}
}

// resolve returns the displayed fg/bg; bgSet is false when the cell inherits the page background.
func (st *sgrState) resolve(defFg, defBg rgbValue) (fg, bg rgbValue, bgSet bool) {
	fg, bg = defFg, defBg
	if st.fgSet {
		fg = st.fg
	}
	if st.bgSet {
		bg, bgSet = st.bg, true
	}
	if st.reverse {
		fg, bg = bg, fg
		bgSet = st.fgSet
	}
	return fg, bg, bgSet
}

func atoiSafe(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// walkCells returns every visible non-space rune with the colors it was emitted with.
func walkCells(s string, defFg, defBg rgbValue) []cellRecord {
	var st sgrState
	var out []cellRecord
	for i := 0; i < len(s); {
		rest := s[i:]
		if m := sgrPrefix.FindStringSubmatch(rest); m != nil {
			st.apply(m[1])
			i += len(m[0])
			continue
		}
		if m := oscSeq.FindString(rest); m != "" {
			i += len(m)
			continue
		}
		if m := csiOther.FindString(rest); m != "" {
			i += len(m)
			continue
		}
		r, size := utf8.DecodeRuneInString(rest)
		i += size
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		fg, bg, bgSet := st.resolve(defFg, defBg)
		out = append(out, cellRecord{fg: fg, bg: bg, bgSet: bgSet, ch: r})
	}
	return out
}

func relativeLuminance(c rgbValue) float64 {
	lin := func(v int) float64 {
		x := float64(v) / 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.r) + 0.7152*lin(c.g) + 0.0722*lin(c.b)
}

func contrast(a, b rgbValue) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func hexValue(t *testing.T, hex string) rgbValue {
	t.Helper()
	var c rgbValue
	_, err := fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%02x%02x%02x", &c.r, &c.g, &c.b)
	require.NoError(t, err, hex)
	return c
}

// componentRow summarizes one rendered surface. Cells without an emitted
// background are measured against the page background and counted separately.
type componentRow struct {
	minRatio  float64
	failing   int
	assumedBG int
	worst     string
	cells     int
}

func summarize(cells []cellRecord, defBg rgbValue) componentRow {
	row := componentRow{minRatio: math.MaxFloat64}
	for _, c := range cells {
		row.cells++
		if !c.bgSet {
			row.assumedBG++
		}
		bg := c.bg
		if !c.bgSet {
			bg = defBg
		}
		ratio := contrast(c.fg, bg)
		if ratio < row.minRatio {
			row.minRatio, row.worst = ratio, string(c.ch)
		}
		if ratio < componentTextMin {
			row.failing++
		}
	}
	if row.cells == 0 {
		row.minRatio = 0
	}
	return row
}

func TestComponentRenderedArtifacts(t *testing.T) { //nolint:paralleltest // ApplyTheme mutates style globals.
	dir := os.Getenv("DOCKER_NEXT_COMPONENT_DIR")
	if dir == "" {
		t.Skip("DOCKER_NEXT_COMPONENT_DIR not set")
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	var tsv strings.Builder
	tsv.WriteString("theme\tcomponent\tsurface\tcells\tassumed_page_bg_cells\tmin_ratio\tfailing_below_4.5\tworst_glyph\tstatus\n")
	record := func(ref, component, surface string, row componentRow) {
		isNew := ref == styles.DefaultThemeRef || ref == styles.DockerNextLightThemeRef
		status := "PASS"
		if row.failing > 0 {
			status = "FLAG"
			if strings.HasPrefix(component, "primary-marker") {
				status = "PRIMARY-FLAG"
				// Regression gate: primary body must hold 4.5 on Docker Next surfaces.
				if isNew {
					t.Errorf("%s %s %s: primary body %.2f < %.1f after dim", ref, component, surface, row.minRatio, componentTextMin)
				}
			}
		}
		fmt.Fprintf(&tsv, "%s\t%s\t%s\t%d\t%d\t%.2f\t%d\t%s\t%s\n", ref, component, surface, row.cells, row.assumedBG, row.minRatio, row.failing, row.worst, status)
	}
	write := func(name, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}

	refs := []string{styles.DefaultThemeRef, styles.DockerNextLightThemeRef, styles.LegacyDefaultThemeRef}
	for _, ref := range refs {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		defFg := hexValue(t, theme.Colors.TextPrimary)
		page := hexValue(t, theme.Colors.Background)

		// Transcript markdown on the page background.
		md, err := markdown.NewRendererWithoutCopyIcon(80).Render(componentSampleMarkdown)
		require.NoError(t, err)
		write(ref+"-markdown.ans", md)
		record(ref, "markdown", "page", summarize(walkCells(md, defFg, page), page))

		// Focused and blurred editor (blurred empty shows the placeholder).
		focused := editor.New(nil)
		focused.SetSize(60, 4)
		focused.SetValue("body text sample")
		focused.Focus()
		fv := focused.View()
		write(ref+"-editor-focused.ans", fv)
		record(ref, "editor-focused", "editor", summarize(walkCells(fv, defFg, hexValue(t, theme.Colors.EditorBg)), hexValue(t, theme.Colors.EditorBg)))

		blurred := editor.New(nil)
		blurred.SetSize(60, 4)
		blurred.Blur()
		bv := blurred.View()
		write(ref+"-editor-placeholder.ans", bv)
		record(ref, "editor-placeholder", "editor", summarize(walkCells(bv, defFg, hexValue(t, theme.Colors.EditorBg)), hexValue(t, theme.Colors.EditorBg)))

		// Picker with the three refs under review plus one other built-in.
		var choices []dialog.ThemeChoice
		for _, r := range []string{styles.DefaultThemeRef, styles.DockerNextLightThemeRef, styles.LegacyDefaultThemeRef, "nord"} {
			th, err := styles.LoadTheme(r)
			require.NoError(t, err)
			choices = append(choices, dialog.ThemeChoice{Ref: r, Name: th.Name, IsCurrent: r == ref, IsBuiltin: true})
		}
		picker := dialog.NewThemePickerDialog(choices, ref)
		picker.SetSize(90, 24)
		pv := picker.View()
		write(ref+"-picker.ans", pv)
		record(ref, "picker", "dialog", summarize(walkCells(pv, defFg, page), page))

		// Dim exactly as production does: NewFadeContext (page background and TextPrimary
		// destination) at alpha .62, applied to real editor and markdown lines.
		fc := styles.NewFadeContext()
		fadeAll := func(src string) string {
			var b strings.Builder
			for line := range strings.SplitSeq(src, "\n") {
				b.WriteString(styles.FadeLineCtx(line, 0.62, &fc))
				b.WriteString("\n")
			}
			return b.String()
		}
		fadedMD := fadeAll(md)
		write(ref+"-fade-markdown.ans", fadedMD)
		record(ref, "fade-0.62", "page", summarize(walkCells(fadedMD, defFg, page), page))
		fadedEditor := fadeAll(fv)
		write(ref+"-fade-editor.ans", fadedEditor)
		record(ref, "fade-0.62", "editor", summarize(walkCells(fadedEditor, defFg, hexValue(t, theme.Colors.EditorBg)), hexValue(t, theme.Colors.EditorBg)))

		// Marker-only primary body: the only visible text is PRIMARYBODY, so the
		// 4.5 check is not diluted by secondary chrome such as borders or hints.
		markerMD, err := markdown.NewRendererWithoutCopyIcon(80).Render("PRIMARYBODY")
		require.NoError(t, err)
		markerEditor := editor.New(nil)
		markerEditor.SetSize(60, 4)
		markerEditor.SetValue("PRIMARYBODY")
		markerEditor.Focus()
		markerView := markerEditor.View()
		fadedMarkerMD := fadeAll(markerMD)
		fadedMarkerEditor := fadeAll(markerView)
		write(ref+"-primary-marker-markdown.ans", fadedMarkerMD)
		write(ref+"-primary-marker-editor.ans", fadedMarkerEditor)
		record(ref, "primary-marker-faded", "page", summarize(walkCells(fadedMarkerMD, defFg, page), page))
		record(ref, "primary-marker-faded", "editor", summarize(walkCells(fadedMarkerEditor, defFg, hexValue(t, theme.Colors.EditorBg)), hexValue(t, theme.Colors.EditorBg)))

		// Message-card composition: the production RenderComposite path over real markdown, then dim.
		cardStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(theme.Colors.TextPrimary)).Background(lipgloss.Color(theme.Colors.CardBg))
		cardBG := hexValue(t, theme.Colors.CardBg)
		fadedCardMD := fadeAll(styles.RenderComposite(cardStyle, md))
		write(ref+"-fade-card.ans", fadedCardMD)
		record(ref, "fade-0.62", "card", summarize(walkCells(fadedCardMD, defFg, cardBG), cardBG))
		fadedCardMarker := fadeAll(styles.RenderComposite(cardStyle, markerMD))
		write(ref+"-primary-marker-card.ans", fadedCardMarker)
		record(ref, "primary-marker-faded", "card", summarize(walkCells(fadedCardMarker, defFg, cardBG), cardBG))

		// Accent text on the selection tint, through the real lipgloss renderer.
		accentOnSelected := lipgloss.NewStyle().Foreground(lipgloss.Color(theme.Colors.Accent)).Background(lipgloss.Color(theme.Colors.Selected)).Render("accent")
		selBG := hexValue(t, theme.Colors.Selected)
		record(ref, "accent-on-selected", "selection", summarize(walkCells(accentOnSelected, defFg, selBG), selBG))
	}
	write("components.tsv", tsv.String())
}
