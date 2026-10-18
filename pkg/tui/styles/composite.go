package styles

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/concurrent"
)

// ANSI reset sequences we need to handle
const (
	resetFull  = "\x1b[0m"
	resetShort = "\x1b[m"
)

// styleSeqCache caches the style sequence for common styles.
// The cache maps a style's string representation to its escape sequence.
var styleSeqCache concurrent.Map[string, string]

// clearStyleSeqCache clears the style sequence cache.
// Called when the theme changes to ensure styles are re-computed with new colors.
func clearStyleSeqCache() {
	styleSeqCache.Clear()
}

// getStyleSeq returns the ANSI escape sequence for a style's colors only.
// Results are cached for repeated calls with the same style.
func getStyleSeq(style lipgloss.Style) string {
	// Use the style's rendered empty string as cache key
	// This is a simple way to identify the style
	cacheKey := style.Render("")

	if seq, ok := styleSeqCache.Load(cacheKey); ok {
		return seq
	}

	// Compute the style sequence
	cleanStyle := style.
		UnsetPadding().
		UnsetMargins().
		UnsetWidth().
		UnsetHeight().
		UnsetBold().
		UnsetItalic().
		UnsetUnderline().
		UnsetStrikethrough().
		UnsetReverse().
		UnsetBlink().
		UnsetFaint().
		UnsetBorderStyle().
		UnsetBorderTop().
		UnsetBorderBottom().
		UnsetBorderLeft().
		UnsetBorderRight().
		UnsetBorderForeground().
		UnsetBorderBackground()
	styleSeq := cleanStyle.Render("")
	styleSeq = strings.TrimSuffix(styleSeq, resetFull)
	styleSeq = strings.TrimSuffix(styleSeq, resetShort)

	styleSeqCache.Store(cacheKey, styleSeq)

	return styleSeq
}

// RenderComposite renders the content with the given style, but ensures that
// any ANSI reset codes in the content are replaced with the style's active sequences,
// preventing the style's background/foreground from being interrupted.
func RenderComposite(style lipgloss.Style, content string) string {
	// Fast path: if content has no reset sequences, just render normally
	if !strings.Contains(content, "\x1b[") {
		return style.Render(content)
	}

	// Get the cached style sequence
	styleSeq := getStyleSeq(style)

	// Replace full resets with the parent's foreground/background, and restore
	// the card background after nested output background changes and resets.
	modifiedContent := strings.ReplaceAll(content, resetFull, resetFull+styleSeq)
	modifiedContent = strings.ReplaceAll(modifiedContent, resetShort, resetFull+styleSeq)
	modifiedContent = restoreCompositeBackground(style, modifiedContent)
	return style.Render(modifiedContent)
}

func restoreCompositeBackground(style lipgloss.Style, content string) string {
	background := style.GetBackground()
	if background == nil {
		return content
	}
	backgroundStyle := lipgloss.NewStyle().Background(background)
	backgroundSequence := backgroundStyle.Render("")
	backgroundSequence = strings.TrimSuffix(strings.TrimSuffix(backgroundSequence, resetFull), resetShort)
	if backgroundSequence == "" {
		return content
	}

	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var output strings.Builder
	var state byte
	for rest := content; rest != ""; {
		sequence, _, consumed, next := ansi.DecodeSequence(rest, state, parser)
		if consumed == 0 {
			break
		}
		output.WriteString(rest[:consumed])
		if ansi.HasCsiPrefix(sequence) && parser.Command() == 'm' && sgrSetsBackground(parser.Params()) {
			output.WriteString(backgroundSequence)
		}
		state, rest = next, rest[consumed:]
	}
	return output.String()
}

func sgrSetsBackground(params []ansi.Param) bool {
	for _, param := range params {
		if code := param.Param(0); code == 48 || code == 49 {
			return true
		}
	}
	return false
}
