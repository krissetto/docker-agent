package editor

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	bannerContentOffset        = styles.AppPadding
	contextBarChevronExpanded  = "▾"
	contextBarChevronCollapsed = "▸"
	contextBarMarginTop        = 1
)

// contextBar renders the expandable bar above the editor that displays
// attachment pills.
type contextBar struct {
	attachments []bannerItem
	height      int
	regions     []bannerRegion
	expanded    bool
	focused     bool
}

type bannerItem struct {
	label       string
	placeholder string
}

type bannerRegion struct {
	start int
	end   int
	y     int
	item  bannerItem
}

func newContextBar() *contextBar {
	return &contextBar{}
}

func (b *contextBar) SetItems(items []bannerItem) {
	b.attachments = items
	b.regions = nil
	if len(items) == 0 {
		b.focused = false
	}
	b.updateHeight()
}

func (b *contextBar) Height() int {
	return b.height
}

func (b *contextBar) IsExpanded() bool {
	return b.expanded
}

func (b *contextBar) Toggle() {
	b.expanded = !b.expanded
	b.regions = nil
	b.updateHeight()
}

func (b *contextBar) SetFocused(focused bool) {
	b.focused = focused && b.hasContent()
}

func (b *contextBar) hasContent() bool {
	return len(b.attachments) > 0
}

func (b *contextBar) updateHeight() {
	if !b.hasContent() {
		b.height = 0
		return
	}
	// top border + summary row
	b.height = 2 + contextBarMarginTop
	if b.expanded {
		b.height += len(b.attachments)
	}
}

func (b *contextBar) View(totalWidth int) string {
	if !b.hasContent() {
		return ""
	}

	innerWidth := max(0, totalWidth-2*styles.AppPadding)
	b.updateHeight()

	var rows []string
	b.regions = nil
	rows = append(rows, b.renderTopBorder(innerWidth), b.renderSummaryRow(innerWidth))
	if b.expanded {
		for i, item := range b.attachments {
			pill := ansi.Truncate(renderAttachmentPill(item), innerWidth, "…")
			rows = append(rows, pill+strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(pill))))
			b.regions = append(b.regions, bannerRegion{start: 0, end: ansi.StringWidth(pill), y: contextBarMarginTop + 2 + i, item: item})
		}
	}

	content := strings.Join(rows, "\n")
	padStyle := lipgloss.NewStyle().Padding(0, styles.AppPadding).MarginTop(contextBarMarginTop)
	if b.focused {
		padStyle = padStyle.Background(styles.Selected)
	}
	return padStyle.Render(content)
}

func (b *contextBar) renderTopBorder(innerWidth int) string {
	if innerWidth <= 0 {
		return ""
	}
	return styles.ResizeHandleStyle.Render(strings.Repeat("─", innerWidth))
}

// renderSummaryRow renders the single collapsed row with attachment pills on the left
// and summary labels (attachment count) on the right.
func (b *contextBar) renderSummaryRow(innerWidth int) string {
	var pills []string
	if !b.expanded {
		for _, item := range b.attachments {
			pills = append(pills, renderAttachmentPill(item))
		}
	}
	left := strings.Join(pills, "  ")

	// Build right side: summary labels
	var rightParts []string
	if len(b.attachments) > 0 {
		countLabel := fmt.Sprintf("%d attachment", len(b.attachments))
		if len(b.attachments) != 1 {
			countLabel += "s"
		}
		rightParts = append(rightParts, styles.AttachmentSizeStyle.Render(countLabel))
	}

	chevron := contextBarChevronCollapsed
	if b.expanded {
		chevron = contextBarChevronExpanded
	}
	right := strings.Join(rightParts, "  ") + " " + styles.MutedStyle.Render(chevron)

	right = ansi.Truncate(right, innerWidth, "")
	leftBudget := max(0, innerWidth-ansi.StringWidth(right)-2)
	left = ansi.Truncate(left, leftBudget, "…")
	if !b.expanded {
		b.buildRegions(pills, "  ")
		for i := range b.regions {
			b.regions[i].end = min(b.regions[i].end, leftBudget)
		}
	}
	return b.alignRow(left, right, innerWidth)
}

// alignRow left-aligns the main content and right-aligns the summary label within the given width.
func (b *contextBar) alignRow(left, right string, innerWidth int) string {
	leftWidth := ansi.StringWidth(left)
	rightWidth := ansi.StringWidth(right)
	gap := innerWidth - leftWidth - rightWidth
	gap = max(gap, 0)
	return left + strings.Repeat(" ", gap) + right
}

func (b *contextBar) buildRegions(pills []string, separator string) {
	b.regions = b.regions[:0]
	if len(pills) == 0 {
		return
	}

	pos := 0
	sepWidth := ansi.StringWidth(separator)

	for i, pill := range pills {
		if i > 0 {
			pos += sepWidth
		}
		width := ansi.StringWidth(pill)
		b.regions = append(b.regions, bannerRegion{
			start: pos,
			end:   pos + width,
			item:  b.attachments[i],
			y:     contextBarMarginTop + 1,
		})
		pos += width
	}
}

func renderAttachmentPill(item bannerItem) string {
	name, size := parseLabel(item.label)
	pill := styles.AttachmentIconStyle.Render("📎 ") + styles.AttachmentBadgeStyle.Render(name)
	if size != "" {
		pill += " " + styles.AttachmentSizeStyle.Render(size)
	}
	return pill
}

func (b *contextBar) HitTest(x int) (bannerItem, bool) {
	return b.HitTestPosition(x, contextBarMarginTop+1)
}

func (b *contextBar) HitTestPosition(x, y int) (bannerItem, bool) {
	if len(b.regions) == 0 {
		return bannerItem{}, false
	}

	rel := x - bannerContentOffset
	if rel < 0 {
		return bannerItem{}, false
	}

	for _, region := range b.regions {
		if y == region.y && rel >= region.start && rel < region.end {
			return region.item, true
		}
	}
	return bannerItem{}, false
}

// parseLabel splits a label like "paste-1 (21.1 KB)" into name and size parts.
func parseLabel(label string) (name, size string) {
	idx := strings.LastIndex(label, " (")
	if idx > 0 && strings.HasSuffix(label, ")") {
		return label[:idx], label[idx+1:]
	}
	return label, ""
}
