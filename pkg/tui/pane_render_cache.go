package tui

import (
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
	raw           string
	width, height int
	theme, colors uint64
	spans         []paneRowSpan
	builds        uint64
}

type panePreparedRow struct{ raw, rendered string }

func (c *paneRenderCache) prepare(key, raw string, width, height int) []paneRowSpan {
	if c.parts == nil {
		c.parts = make(map[string]*panePreparedPart)
	}
	part := c.parts[key]
	if part == nil {
		part = &panePreparedPart{}
		c.parts[key] = part
	}
	theme, colors := styles.ThemeGeneration(), styles.AgentColorGeneration()
	if part.spans != nil && part.raw == raw && part.width == width && part.height == height && part.theme == theme && part.colors == colors {
		return part.spans
	}
	part.raw, part.width, part.height, part.theme, part.colors = raw, width, height, theme, colors
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
		raw := assemblePaneRow(spans, width)
		if c.rows[y].raw != raw {
			c.rows[y] = panePreparedRow{raw: raw, rendered: trimPaneDefaultPadding(raw)}
		}
		lines[y] = c.rows[y].rendered
	}
	return strings.Join(lines, "\n")
}

type paneTitleKey struct {
	name, nodeID, title, activity string
	width, children               int
	focused, dimmed               bool
	theme, colors                 uint64
}
type paneTitleEntry struct {
	key      paneTitleKey
	rendered string
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
