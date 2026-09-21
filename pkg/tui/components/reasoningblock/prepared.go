package reasoningblock

import (
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type PreparedReasoning struct {
	width, version     int
	theme              uint64
	selected, expanded bool
	chunks             map[string]string
	cache              *renderCache
}

func (m *Model) preparedValid() bool {
	p := m.prepared
	return p != nil && p.width == m.contentWidth() && p.version == m.reasoningVersion && p.theme == styles.ThemeGeneration() && p.selected == m.selected && p.expanded == m.expanded
}

// PrepareReasoning captures immutable inputs; the returned worker never reads
// the block, tool views, theme globals, or animation state.
func (m *Model) PrepareReasoning() func() *PreparedReasoning {
	if m.preparedValid() {
		return nil
	}
	size := 0
	for _, item := range m.contentItems {
		size += len(item.reasoning)
	}
	if size < 16*1024 {
		return nil
	}
	items := slices.Clone(m.contentItems)
	width := m.contentWidth()
	renderer := markdown.NewFastRenderer(width).HideCopyIcon().FreezeStyles()
	muted, envelope := styles.MutedStyle.Italic(true), m.messageStyle()
	prepared := PreparedReasoning{width: width, version: m.reasoningVersion, theme: styles.ThemeGeneration(), selected: m.selected, expanded: m.expanded, chunks: make(map[string]string)}
	toolCount := len(m.toolEntries)
	return func() *PreparedReasoning {
		var reasoning []string
		for _, item := range items {
			if item.kind == contentItemReasoning {
				reasoning = append(reasoning, item.reasoning)
			}
		}
		joined := strings.Join(reasoning, "\n\n")
		rendered, _ := renderer.Render(joined)
		lines := strings.Split(strings.TrimRight(ansi.Strip(rendered), "\n\r\t "), "\n")
		prepared.cache = reasoningPreviewCache(lines, width, prepared.version, prepared.theme, toolCount)
		if prepared.expanded {
			for _, text := range reasoning {
				rendered, _ := renderer.Render(text)
				clean := strings.TrimRight(ansi.Strip(rendered), "\n\r\t ")
				prepared.chunks[text] = envelope.Render(muted.Render(clean))
			}
		}
		return &prepared
	}
}

func (m *Model) ApplyPreparedReasoning(prepared *PreparedReasoning) bool {
	m.prepared = prepared
	if !m.preparedValid() {
		m.prepared = nil
		return false
	}
	m.cache = prepared.cache
	return true
}

func reasoningPreviewCache(lines []string, width, version int, theme uint64, tools int) *renderCache {
	hasExtra := tools > 0 || len(lines) > previewLines
	preview := make([]string, 0, previewLines)
	nonblank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonblank++
		if len(preview) == previewLines {
			copy(preview, preview[1:])
			preview = preview[:previewLines-1]
		}
		preview = append(preview, line)
	}
	for i := range preview {
		preview[i] = strings.Clone(preview[i])
	}
	return &renderCache{themeGeneration: theme, width: width, reasoningVersion: version, lines: preview, hasExtra: hasExtra, truncated: nonblank > previewLines, hasLines: len(lines) > 0}
}
