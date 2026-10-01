package messages

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
)

var underlineStyle = lipgloss.NewStyle().Underline(true)

// hoveredURL tracks the URL currently under the mouse cursor.
type hoveredURL struct {
	line     int // global rendered line
	startCol int // display column where URL starts
	endCol   int // display column where URL ends (exclusive)
}

// urlSpanCache caches parsed URL spans per rendered line index.
// Cleared when renderedLines changes (renderDirty rebuild).
type urlSpanCache struct {
	spans      map[int][]urlSpan
	references map[int]renderedReference
}

func newURLSpanCache() *urlSpanCache {
	return &urlSpanCache{spans: make(map[int][]urlSpan), references: make(map[int]renderedReference)}
}

// get returns the cached URL spans for the given line, parsing on first access.
func (c *urlSpanCache) get(line int, renderedLine string) []urlSpan {
	if spans, ok := c.spans[line]; ok {
		return spans
	}
	spans := findAllURLSpans(renderedLine)
	c.spans[line] = spans
	return spans
}

// clear resets the cache (called when rendered lines change).
func (c *urlSpanCache) clear() {
	c.spans = make(map[int][]urlSpan)
	c.references = make(map[int]renderedReference)
}

// urlAtPosition extracts a URL from the rendered line at the given display column.
// Returns the URL string if found, or empty string if the click position is not on a URL.
func urlAtPosition(renderedLine string, col int) string {
	if renderedLine == "" {
		return ""
	}
	for _, span := range findAllURLSpans(renderedLine) {
		if col >= span.startCol && col < span.endCol {
			if span.url != agentidentity.Link {
				return span.url
			}
			return ""
		}
	}
	return ""
}

type urlSpan struct {
	url      string
	startCol int // display column where URL starts
	endCol   int // display column where URL ends (exclusive)
}

// extractOSC8Links finds OSC 8 hyperlinks in a rendered line and returns
// their URL + display column positions. The display columns correspond to
// the visible text after stripping ANSI sequences.
func extractOSC8Links(renderedLine string) []urlSpan {
	var spans []urlSpan
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	col, start := 0, 0
	active := ""
	for renderedLine != "" {
		sequence, width, consumed, next := ansi.DecodeSequence(renderedLine, state, parser)
		if strings.HasPrefix(sequence, "\x1b]8;") {
			if active != "" && col > start {
				spans = append(spans, urlSpan{url: active, startCol: start, endCol: col})
			}
			payload := strings.TrimSuffix(strings.TrimSuffix(sequence[4:], "\x07"), "\x1b\\")
			_, active, _ = strings.Cut(payload, ";")
			start = col
		}
		col += width
		state, renderedLine = next, renderedLine[consumed:]
	}
	if active != "" && col > start {
		spans = append(spans, urlSpan{url: active, startCol: start, endCol: col})
	}

	return spans
}

// findAllURLSpans finds all clickable URLs in a rendered line by combining:
// 1. OSC 8 hyperlinks (from invisible sequences in the rendered line)
// 2. Visible URLs (from plain text detection)
// OSC 8 links take priority when they overlap with visible URL spans.
func findAllURLSpans(renderedLine string) []urlSpan {
	osc8Spans := extractOSC8Links(renderedLine)
	plainLine := ansi.Strip(renderedLine)
	visibleSpans := findURLSpans(plainLine)

	if len(osc8Spans) == 0 {
		return visibleSpans
	}
	if len(visibleSpans) == 0 {
		return osc8Spans
	}

	// Merge: OSC 8 spans take priority. Remove visible spans that overlap.
	merged := make([]urlSpan, 0, len(osc8Spans)+len(visibleSpans))
	merged = append(merged, osc8Spans...)
	for _, vs := range visibleSpans {
		overlaps := false
		for _, os := range osc8Spans {
			if vs.startCol < os.endCol && vs.endCol > os.startCol {
				overlaps = true
				break
			}
		}
		if !overlaps {
			merged = append(merged, vs)
		}
	}
	return merged
}

// findURLSpans finds all URLs in plain text and returns their display column ranges.
func findURLSpans(text string) []urlSpan {
	var spans []urlSpan
	runes := []rune(text)
	n := len(runes)

	for i := 0; i < n; {
		// Look for http:// or https://
		remaining := string(runes[i:])
		var prefixLen int
		switch {
		case strings.HasPrefix(remaining, "https://"):
			prefixLen = len("https://")
		case strings.HasPrefix(remaining, "http://"):
			prefixLen = len("http://")
		default:
			i++
			continue
		}

		// Must not be preceded by a word character (avoid matching mid-word)
		if i > 0 && isURLWordChar(runes[i-1]) {
			i++
			continue
		}

		urlStart := i
		j := i + prefixLen
		// Extend to cover the URL body
		for j < n && isURLChar(runes[j]) {
			j++
		}
		// Strip common trailing punctuation that's unlikely part of the URL
		for j > urlStart+prefixLen && isTrailingPunct(runes[j-1]) {
			j--
		}
		// Balance parentheses: strip trailing ')' only if unmatched
		url := string(runes[urlStart:j])
		url = balanceParens(url)
		j = urlStart + len([]rune(url))

		startCol := runeSliceWidth(runes[:urlStart])
		endCol := startCol + runeSliceWidth(runes[urlStart:j])

		spans = append(spans, urlSpan{
			url:      url,
			startCol: startCol,
			endCol:   endCol,
		})
		i = j
	}
	return spans
}

func runeSliceWidth(runes []rune) int {
	return ansi.StringWidth(string(runes))
}

func isURLChar(r rune) bool {
	if r <= ' ' || r == '"' || r == '<' || r == '>' || r == '{' || r == '}' || r == '|' || r == '\\' || r == '^' || r == '`' {
		return false
	}
	return true
}

func isURLWordChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isTrailingPunct(r rune) bool {
	return r == '.' || r == ',' || r == ';' || r == ':' || r == '!' || r == '?'
}

// balanceParens strips a trailing ')' if there are more closing than opening parens.
// This handles the common case of URLs wrapped in parentheses like (https://example.com).
func balanceParens(url string) string {
	if !strings.HasSuffix(url, ")") {
		return url
	}
	open := strings.Count(url, "(")
	if strings.Count(url, ")") > open {
		return url[:len(url)-1]
	}
	return url
}

// urlAt returns the URL at the given global line and display column, or empty string.
func (m *model) urlAt(line, col int) string {
	m.ensureAllItemsRendered()
	if line < 0 || line >= m.totalHeight {
		return ""
	}
	rendered := m.renderedLine(line)
	for _, span := range m.urlSpans.get(line, rendered) {
		if col >= span.startCol && col < span.endCol {
			if span.url != agentidentity.Link {
				return span.url
			}
			return ""
		}
	}
	return ""
}

// updateHoveredURL updates the hovered URL state based on mouse position.
func (m *model) updateHoveredURL(line, col int) {
	m.ensureAllItemsRendered()

	if line >= 0 && line < m.totalHeight {
		rendered := m.renderedLine(line)
		for _, span := range m.urlSpans.get(line, rendered) {
			if col >= span.startCol && col < span.endCol {
				if span.url == agentidentity.Link {
					if ref := m.renderedReference(line); ref.key.message != nil {
						m.setReferenceHover(ref.key)
						if m.hoveredURL != nil {
							m.hoveredURL = nil
							m.invalidateView()
						}
						return
					}
					continue
				}
				m.setReferenceHover(referenceHoverKey{})
				newHover := &hoveredURL{line: line, startCol: span.startCol, endCol: span.endCol}
				if m.hoveredURL == nil || *m.hoveredURL != *newHover {
					m.hoveredURL = newHover
					m.invalidateView()
				}
				return
			}
		}
	}

	m.setReferenceHover(referenceHoverKey{})
	if m.hoveredURL != nil {
		m.hoveredURL = nil
		m.invalidateView()
	}
}

// applyURLUnderline underlines the hovered URL in the visible lines.
func (m *model) applyURLUnderline(lines []string, viewportStartLine int) []string {
	if m.hoveredURL == nil {
		return lines
	}

	viewIdx := m.hoveredURL.line - viewportStartLine
	if viewIdx < 0 || viewIdx >= len(lines) {
		return lines
	}

	result := make([]string, len(lines))
	copy(result, lines)
	style := underlineStyle

	result[viewIdx] = styleLineSegment(lines[viewIdx], m.hoveredURL.startCol, m.hoveredURL.endCol, style)
	return result
}
