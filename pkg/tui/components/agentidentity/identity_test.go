package agentidentity

import (
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestIdentityOnlyNameUsesAccentAndHover(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "72690-full-node", Name: "director (lead) 界", Agent: "director", DisplayID: "72690"}
	labelWidth := ansi.StringWidth(ref.Label())
	nameWidth := ansi.StringWidth(ref.Name)
	for _, width := range []int{4, nameWidth, nameWidth + 3, labelWidth} {
		label := Label(ref, width)
		hover := Hover(label, 0, width, ref)
		require.Equal(t, ansi.Strip(label), ansi.Strip(hover))
		assert.Contains(t, label, nameLink)
		assert.Contains(t, hover, nameLink)
		for x := range ansi.StringWidth(label) {
			wantNormal, wantHover := color.RGBAModel.Convert(styles.MutedStyle.GetForeground()), color.RGBAModel.Convert(styles.MutedStyle.GetForeground())
			if x < nameWidth {
				wantNormal = color.RGBAModel.Convert(styles.AgentIdentityStyle(ref.Agent, false).GetForeground())
				wantHover = color.RGBAModel.Convert(styles.AgentIdentityStyle(ref.Agent, true).GetForeground())
			}
			assert.Equal(t, wantNormal, foregroundAt(label, x), "normal width=%d x=%d", width, x)
			assert.Equal(t, wantHover, foregroundAt(hover, x), "hover width=%d x=%d", width, x)
		}
	}
}

func TestIdentityWrappedSuffixStaysNeutralOnHover(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "746aa-exact-node", Name: "worker (lead)", Agent: "worker", DisplayID: "746aa"}
	for _, prefix := range []string{"Spawned ", "Inspecting ", ""} {
		wrapped := Wrap(styles.MutedStyle.Render(prefix), ref, styles.MutedStyle.Render(" has replied"), 18)
		for line := range strings.SplitSeq(wrapped, "\n") {
			hover := Hover(line, 0, ansi.StringWidth(line), ref)
			require.Equal(t, ansi.Strip(line), ansi.Strip(hover))
			for x := range ansi.StringWidth(line) {
				before, after := foregroundAt(line, x), foregroundAt(hover, x)
				if before == color.RGBAModel.Convert(styles.AgentIdentityStyle(ref.Agent, false).GetForeground()) {
					assert.Equal(t, color.RGBAModel.Convert(styles.AgentIdentityStyle(ref.Agent, true).GetForeground()), after)
				} else {
					assert.Equal(t, before, after, "neutral suffix/connector cannot brighten")
				}
			}
			assert.Equal(t, strings.Count(line, ansi.SetHyperlink(Link)), strings.Count(hover, ansi.SetHyperlink(Link)))
		}
	}
}

func TestIdentityBorderKeepsNeutralSuffix(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "1819e-exact-node", Name: "root", Agent: "root", DisplayID: "1819e"}
	style := styles.UserMessageStyle
	line := strings.Split(Border(style.Width(40).Render("body"), ref, 40, style), "\n")[0]
	start := style.GetBorderLeftSize() + style.GetPaddingLeft()
	hover := Hover(line, start, start+ansi.StringWidth(ref.Label()), ref)
	for x := start + len(ref.Name); x < start+ansi.StringWidth(ref.Label()); x++ {
		assert.Equal(t, color.RGBAModel.Convert(styles.MutedStyle.GetForeground()), foregroundAt(line, x))
		assert.Equal(t, foregroundAt(line, x), foregroundAt(hover, x))
	}
}

func foregroundAt(line string, col int) color.Color {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var fg color.Color
	var state byte
	for w := 0; line != "" && w <= col; {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			params := p.Params()
			if len(params) == 0 {
				fg = nil
			}
			for i := 0; i < len(params); i++ {
				switch param := params[i].Param(0); {
				case param == 0 || param == 39:
					fg = nil
				case param == 38 || param == 48:
					var c color.Color
					n := ansi.ReadStyleColor(params[i:], &c)
					if n > 0 {
						if param == 38 {
							fg = c
						}
						i += n - 1
					}
				case param >= 30 && param <= 37:
					fg = ansi.Black + ansi.BasicColor(param-30)
				case param >= 90 && param <= 97:
					fg = ansi.BrightBlack + ansi.BasicColor(param-90)
				}
			}
		}
		w += width
		state, line = next, line[n:]
	}
	if fg == nil {
		return nil
	}
	return color.RGBAModel.Convert(fg)
}

func TestHoverProgressRebasesNameOnlyAcrossWarmThemes(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "complete-canonical-node-id", Name: "director 界", Agent: "director", DisplayID: "complete-canonical-node-id"}
	for _, themeRef := range []string{"default", "default-light", "nord"} {
		theme, err := styles.LoadTheme(themeRef)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, line := range []string{Label(ref, 80), strings.Split(Border(styles.UserMessageStyle.Width(80).Render("body"), ref, 80, styles.UserMessageStyle), "\n")[0]} {
			require.Equal(t, line, HoverProgress(line, 0, ansi.StringWidth(line), ref, 0))
			got := HoverProgress(line, 0, ansi.StringWidth(line), ref, .5)
			assert.Equal(t, ansi.Strip(line), ansi.Strip(got))
			assert.Equal(t, ansi.StringWidth(line), ansi.StringWidth(got))
			nameAt := strings.Index(ansi.Strip(line), ref.Name)
			require.GreaterOrEqual(t, nameAt, 0)
			start := ansi.StringWidth(ansi.Strip(line)[:nameAt])
			want := styles.Brighten(styles.AgentIdentityStyle(ref.Agent, false).GetForeground(), .125)
			assert.Equal(t, color.RGBAModel.Convert(want), foregroundAt(got, start))
			suffixAt := start + ansi.StringWidth(ref.Name)
			assert.Equal(t, foregroundAt(line, suffixAt), foregroundAt(got, suffixAt))
			assert.Contains(t, got, ansi.SetHyperlink(Link))
			assert.Contains(t, ansi.Strip(got), ref.DisplayID)
		}
	}
}

func TestWrappedSuffixStaysNeutralWhenAgentAndMutedColorsMatch(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	theme := styles.DefaultTheme()
	theme.Colors.Accent = "#808080"
	theme.Colors.TextMuted = "#808080"
	styles.ApplyTheme(theme)
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: "full-canonical-node-id", Name: "X", Agent: "unregistered-collision-agent", DisplayID: "1234567890-full-neutral-id"}
	require.Equal(t, styles.AgentIdentityStyle(ref.Agent, false).GetForeground(), styles.MutedStyle.GetForeground())
	wrapped := Wrap("", ref, "", 8)
	suffixRows := 0
	for line := range strings.SplitSeq(wrapped, "\n") {
		got := HoverProgress(line, 0, ansi.StringWidth(line), ref, 1)
		assert.Equal(t, ansi.Strip(line), ansi.Strip(got))
		if strings.Contains(line, nameLink) {
			continue
		}
		suffixRows++
		assert.Equal(t, line, got, "suffix-only row has no name metadata and must remain byte-identical")
	}
	require.Positive(t, suffixRows)
	label := Label(ref, 80)
	got := Hover(label, 0, ansi.StringWidth(label), ref)
	for x := ansi.StringWidth(ref.Name); x < ansi.StringWidth(label); x++ {
		assert.Equal(t, foregroundAt(label, x), foregroundAt(got, x), "neutral ID cell %d", x)
	}
}

func TestNameMetadataDoesNotSupplyForegroundToUncoloredWrappedCells(t *testing.T) {
	t.Parallel()
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, Name: "worker", Agent: "worker", ID: "full-id"}
	line := nameLink + styles.AgentIdentityStyle(ref.Agent, false).Render("worker") + " " + ansi.ResetHyperlink()
	got := HoverProgress(line, 0, ansi.StringWidth(line), ref, .5)
	require.Equal(t, ansi.Strip(line), ansi.Strip(got))
	last := ansi.StringWidth(line) - 1
	require.Nil(t, foregroundAt(line, last))
	assert.Nil(t, foregroundAt(got, last), "metadata cannot fabricate a foreground on reset/uncolored cells")
}
