package editor

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

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
	attachments     []bannerItem
	height          int
	maxHeight       int
	width           int
	regions         []bannerRegion
	expanded        bool
	focused         bool
	themeGeneration uint64
	view            string
	renders         uint64
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
	return &contextBar{maxHeight: 6}
}

func (b *contextBar) SetItems(items []bannerItem) {
	if slices.Equal(b.attachments, items) {
		return
	}
	b.attachments = slices.Clone(items)
	b.regions = nil
	if len(items) == 0 {
		b.focused = false
	}
	b.updateHeight()
	b.reflow()
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
	b.reflow()
}

func (b *contextBar) SetFocused(focused bool) {
	focused = focused && b.hasContent() && b.height > 0
	if b.focused == focused {
		return
	}
	b.focused = focused
	b.reflow()
}

func (b *contextBar) hasContent() bool {
	return len(b.attachments) > 0
}

func (b *contextBar) updateHeight() {
	if !b.hasContent() || b.maxHeight == 0 {
		b.height = 0
		b.focused = false
		return
	}
	// top border + summary row
	b.height = 2 + contextBarMarginTop
	if b.expanded {
		b.height += len(b.attachments)
	}
	b.height = min(b.height, b.maxHeight)
}

func (b *contextBar) SetMaxHeight(height int) {
	height = max(0, height)
	if b.maxHeight == height {
		return
	}
	b.maxHeight = height
	b.regions = nil
	b.updateHeight()
	b.reflow()
}

func (b *contextBar) summaryY() int {
	return min(2, max(0, b.height-1))
}

// SetSize prepares hit regions and immutable paint input at the owner mutation
// boundary. Painting retained input never changes hit-testing geometry.
func (b *contextBar) SetSize(totalWidth int) {
	width := max(0, totalWidth)
	if b.width == width && b.themeGeneration == styles.ThemeGeneration() {
		return
	}
	b.width = width
	b.reflow()
}

func (b *contextBar) View(totalWidth int) string {
	b.SetSize(totalWidth)
	return b.view
}

func (b *contextBar) reflow() {
	b.renders++
	totalWidth := b.width
	b.themeGeneration = styles.ThemeGeneration()
	b.regions = nil
	if !b.hasContent() || b.height == 0 || totalWidth <= 0 {
		b.view = ""
		return
	}

	leftPadding := min(styles.AppPadding, totalWidth)
	rightPadding := min(styles.AppPadding, totalWidth-leftPadding)
	innerWidth := totalWidth - leftPadding - rightPadding
	var rows []string
	if b.height >= 3 {
		rows = append(rows, strings.Repeat(" ", innerWidth))
	}
	if b.height >= 2 {
		rows = append(rows, b.renderTopBorder(innerWidth))
	}
	rows = append(rows, b.renderSummaryRow(innerWidth))
	if b.expanded {
		for i, item := range b.attachments {
			if len(rows) >= b.height {
				break
			}
			pill := ansi.Truncate(renderAttachmentPill(item), innerWidth, "…")
			rows = append(rows, pill+strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(pill))))
			b.regions = append(b.regions, bannerRegion{start: 0, end: ansi.StringWidth(pill), y: b.summaryY() + 1 + i, item: item})
		}
	}

	padStyle := lipgloss.NewStyle().Padding(0, rightPadding, 0, leftPadding)
	if b.focused {
		padStyle = padStyle.Background(styles.Selected)
	}
	b.view = padStyle.Render(strings.Join(rows, "\n"))
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
			y:     b.summaryY(),
		})
		pos += width
	}
}

func renderAttachmentPill(item bannerItem) string {
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(item.label))
	name, size := parseLabel(label)
	pill := styles.AttachmentIconStyle.Render("📎 ") + styles.AttachmentBadgeStyle.Render(name)
	if size != "" {
		pill += " " + styles.AttachmentSizeStyle.Render(size)
	}
	return pill
}

func (b *contextBar) HitTest(x int) (bannerItem, bool) {
	return b.HitTestPosition(x, b.summaryY())
}

func (b *contextBar) HitTestPosition(x, y int) (bannerItem, bool) {
	if len(b.regions) == 0 || x >= b.width || y < 0 || y >= b.height {
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
