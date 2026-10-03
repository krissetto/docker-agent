package tui

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// paneRenderCache owns only the last bounded viewport for each visible component.
// Read the component before consulting this cache: transcript generations alone
// do not cover every media, selection and banner update. Exact bytes also make
// same-size page replacement safe without coupling this cache to session owners.
type paneRenderCache struct {
	parts map[string]*panePreparedPart
	rows  []panePreparedRow
}

type panePreparedPart struct {
	input  panePreparationInput
	spans  []paneRowSpan
	builds uint64
}

// Preparation consumes exact authoritative bytes and dimensions, not revisions.
// No palette is consulted after this boundary: ANSI paint is already in raw.
type panePreparationInput struct {
	raw           string
	width, height int
}

type panePreparedRow struct {
	spans    []paneRowSpan
	width    int
	rendered string
	valid    bool
	builds   uint64
}

func (r *panePreparedRow) render(spans []paneRowSpan, width int) string {
	if r.valid && r.width == width && slices.Equal(r.spans, spans) {
		return r.rendered
	}
	// Own the complete input before assembly sorts the caller's spans. Never
	// retain the caller's slice: composition may reuse or mutate its metadata.
	// Clear old entries so shrinking rows cannot retain off-viewport strings.
	clear(r.spans)
	r.spans = append(r.spans[:0], spans...)
	r.width = width
	r.rendered = trimPaneDefaultPadding(assemblePaneRow(spans, width))
	r.valid = true
	r.builds++
	return r.rendered
}

func (c *paneRenderCache) prepare(key, raw string, width, height int) []paneRowSpan {
	if c.parts == nil {
		c.parts = make(map[string]*panePreparedPart)
	}
	part := c.parts[key]
	if part == nil {
		part = &panePreparedPart{}
		c.parts[key] = part
	}
	in := panePreparationInput{raw: raw, width: width, height: height}
	if part.spans != nil && part.input == in {
		return part.spans
	}
	part.input = in
	part.spans = make([]paneRowSpan, 0, max(0, height))
	for line := range strings.SplitSeq(raw, "\n") {
		if len(part.spans) >= height {
			break
		}
		content := ansi.Truncate(line, width, "")
		part.spans = append(part.spans, preparedPaneSpan(content, width))
	}
	part.builds++
	return part.spans
}

func preparedPaneSpan(content string, width int) paneRowSpan {
	serialized := content + strings.Repeat(" ", max(0, width-ansi.StringWidth(content)))
	if strings.Contains(content, "\x1b") {
		serialized += "\x1b[m"
		if strings.Contains(content, "\x1b]8;") {
			serialized += ansi.ResetHyperlink()
		}
	}
	return paneRowSpan{width: width, content: content, serialized: serialized, prepared: true}
}

func (c *paneRenderCache) retain(keys map[string]bool) {
	for key := range c.parts {
		if !keys[key] {
			delete(c.parts, key)
		}
	}
}

func (c *paneRenderCache) render(rows [][]paneRowSpan, width int) string {
	if len(c.rows) != len(rows) {
		c.rows = make([]panePreparedRow, len(rows))
	}
	lines := make([]string, len(rows))
	for y, spans := range rows {
		lines[y] = c.rows[y].render(spans, width)
	}
	return strings.Join(lines, "\n")
}

// shellFrameCache keeps one value per fixed shell component, not per session.
// Owners still render their authoritative content; unchanged chrome avoids
// repeated ANSI width scans and framing when a different component animates.
type shellFrameCache struct {
	raw, rendered string
	width, height int
	theme         uint64
	valid         bool
}

func (c *shellFrameCache) render(raw string, width, height int, render func() string) string {
	theme := styles.ThemeGeneration()
	if c.valid && c.raw == raw && c.width == width && c.height == height && c.theme == theme {
		return c.rendered
	}
	*c = shellFrameCache{raw: raw, rendered: render(), width: width, height: height, theme: theme, valid: true}
	return c.rendered
}
