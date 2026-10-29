package toolcommon

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// EffortGaugeCells is the fixed cell width of the effort gauge. The six cells
// map one-to-one onto the six selectable effort levels, so the gauge is
// lossless: the filled-cell count alone identifies the level, and the color
// ramp is a secondary cue.
const EffortGaugeCells = 6

// effortCells maps each selectable effort level to its filled-cell count. The
// mapping is lossless (six levels onto six cells).
var effortCells = map[effort.Level]int{
	effort.Minimal: 1,
	effort.Low:     2,
	effort.Medium:  3,
	effort.High:    4,
	effort.XHigh:   5,
	effort.Max:     6,
}

// EffortFillStyle returns the foreground style for a level's filled cells: a
// low-to-high ramp (muted → accent → highlight → success → warning) kept as a
// secondary cue on top of the now-lossless cell count. Colors come from the
// active theme so the gauge stays theme-aware.
func EffortFillStyle(level effort.Level) lipgloss.Style {
	var c color.Color
	switch level {
	case effort.Minimal:
		c = styles.TextMuted
	case effort.Low:
		c = styles.Accent
	case effort.Medium:
		c = styles.Highlight
	case effort.High, effort.XHigh:
		c = styles.Success
	case effort.Max:
		c = styles.Warning
	default:
		c = styles.TextMuted
	}
	return lipgloss.NewStyle().Foreground(c)
}

// Empty gauge cells are decoration rather than readable prose; their extra
// faint treatment keeps them subordinate to the filled effort level.
func effortEmptyStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(styles.TextMuted).Faint(true)
}

// EffortGauge renders the fixed-width six-cell gauge for an effort level: the
// filled cells take the level's ramp color, the rest are faint. The width is
// constant across levels so the badge column stays aligned wherever the gauge
// is shown (roster rows, focus card, agent-details dialog).
func EffortGauge(level effort.Level) string {
	filled := effortCells[level]
	fillStyle := EffortFillStyle(level)
	emptyStyle := effortEmptyStyle()
	var b strings.Builder
	for i := range EffortGaugeCells {
		if i < filled {
			b.WriteString(fillStyle.Render(styles.GaugeFilled))
		} else {
			b.WriteString(emptyStyle.Render(styles.GaugeEmpty))
		}
	}
	return b.String()
}

// EffortGaugeEmpty renders a fully empty, faint six-cell gauge. It marks a model
// that is capable of thinking but has it switched off ("off"), reading as
// "capable but disabled" — distinct from a non-capable model, which shows no
// gauge at all.
func EffortGaugeEmpty() string {
	return strings.Repeat(effortEmptyStyle().Render(styles.GaugeEmpty), EffortGaugeCells)
}

// ThinkingMarker returns the compact visual marker for a thinking wire label,
// used in the gauge column of the focus card and the agent-details dialog:
//
//   - an effort level → the six-cell effort gauge (colored)
//   - "off"           → a dim empty gauge
//   - "adaptive"      → the muted word "auto"
//   - a token budget  → the token glyph "◉"
//   - an empty label  → "" (no marker)
//
// Unknown/future level words yield "" so the descriptive value still carries
// the meaning.
func ThinkingMarker(label string) string {
	switch label {
	case "":
		return ""
	case "off":
		return EffortGaugeEmpty()
	case "adaptive":
		return styles.MutedStyle.Render("auto")
	}
	if isAllDigits(label) {
		return styles.MutedStyle.Render(styles.TokenGlyph)
	}
	if level, ok := effort.Parse(label); ok {
		return EffortGauge(level)
	}
	return ""
}

// ThinkingGaugeValue renders the shared "<marker> <description>" thinking
// summary used by the sidebar focus card and the agent-details dialog: the
// effort gauge (or compact marker) followed by the human-readable description
// from ThinkingDescription. Returns "" when the label carries no thinking
// configuration. The "off" description uses the semantic muted text color.
func ThinkingGaugeValue(label string) string {
	desc := ThinkingDescription(label)
	if desc == "" {
		return ""
	}
	word := styles.MutedStyle.Render(desc)

	marker := ThinkingMarker(label)
	if marker == "" {
		return word
	}
	return marker + " " + word
}
