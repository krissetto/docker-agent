package editor

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
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
	hoverAnimation  animation.Subscription
	hoverBound      bool
	hoverValues     map[string]bannerHoverValue
	attachments     []bannerItem
	height          int
	maxHeight       int
	width           int
	regions         []bannerRegion
	hidden          []bannerItem
	toggleRegion    bannerRegion
	canExpand       bool
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
	b.cancelHover()
	b.attachments = slices.Clone(items)
	b.regions = nil
	if len(items) == 0 {
		b.focused = false
	}
	b.reflow()
}

func (b *contextBar) Height() int {
	return b.height
}

func (b *contextBar) IsExpanded() bool {
	return b.expanded
}

func (b *contextBar) Toggle() {
	if !b.canExpand {
		return
	}
	b.expanded = !b.expanded
	b.regions = nil
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

func (b *contextBar) SetMaxHeight(height int) {
	height = max(0, height)
	if b.maxHeight == height {
		return
	}
	b.cancelHover()
	b.maxHeight = height
	b.regions = nil
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
	b.cancelHover()
	b.width = width
	b.reflow()
}

func (b *contextBar) View(totalWidth int) string {
	b.SetSize(totalWidth)
	return b.view
}

func (b *contextBar) reflow() {
	b.renders++
	b.themeGeneration = styles.ThemeGeneration()
	b.regions, b.hidden = nil, nil
	b.toggleRegion = bannerRegion{}
	b.canExpand = false
	b.height = 0
	b.view = ""
	if !b.hasContent() || b.maxHeight == 0 {
		b.expanded, b.focused = false, false
		return
	}
	b.height = min(2+contextBarMarginTop, b.maxHeight)
	if b.width <= 0 {
		b.expanded = false
		return
	}

	leftPadding := min(styles.AppPadding, b.width)
	rightPadding := min(styles.AppPadding, b.width-leftPadding)
	innerWidth := b.width - leftPadding - rightPadding
	count := fmt.Sprintf("%d attachment", len(b.attachments))
	if len(b.attachments) != 1 {
		count += "s"
	}
	left, right := b.prepareSummary(innerWidth, count)
	b.canExpand = len(b.hidden) > 0 && b.maxHeight > b.height && ansi.StringWidth(right) > 0
	if !b.canExpand {
		b.expanded = false
	} else {
		chevron := contextBarChevronCollapsed
		if b.expanded {
			chevron = contextBarChevronExpanded
		}
		left, right = b.prepareSummary(innerWidth, count+" "+chevron)
		b.toggleRegion = bannerRegion{start: innerWidth - ansi.StringWidth(right), end: innerWidth, y: b.summaryY()}
		if b.focused {
			right = styles.AttachmentSizeStyle.Underline(true).Render(ansi.Strip(right))
		}
		countWidth := max(0, ansi.StringWidth(right)-2)
		right = styles.HoverText(ansi.Cut(right, 0, countWidth), b.hoverValues["count"].value, styles.TextPrimary) + ansi.Cut(right, countWidth, ansi.StringWidth(right))
	}

	var rows []string
	if b.height >= 3 {
		rows = append(rows, strings.Repeat(" ", innerWidth))
	}
	if b.height >= 2 {
		rows = append(rows, styles.ResizeHandleStyle.Render(strings.Repeat("─", innerWidth)))
	}
	gap := max(0, innerWidth-ansi.StringWidth(left)-ansi.StringWidth(right))
	rows = append(rows, left+strings.Repeat(" ", gap)+right)
	if b.expanded {
		for _, item := range b.hidden {
			if len(rows) >= b.maxHeight {
				break
			}
			pill := ansi.Truncate(b.renderPill(item), innerWidth, "…")
			b.regions = append(b.regions, bannerRegion{start: 0, end: ansi.StringWidth(pill), y: len(rows), item: item})
			rows = append(rows, pill+strings.Repeat(" ", max(0, innerWidth-ansi.StringWidth(pill))))
		}
	}
	b.height = len(rows)
	b.view = lipgloss.NewStyle().Padding(0, rightPadding, 0, leftPadding).Render(strings.Join(rows, "\n"))
}

// prepareSummary retains identities, not a clipped concatenation of pills. The
// first pill may be truncated; expansion only reveals additional attachments.
func (b *contextBar) prepareSummary(innerWidth int, count string) (string, string) {
	b.regions, b.hidden = nil, nil
	// Keep a first attachment visible even when its name needs truncation.
	rightBudget := max(0, innerWidth-min(4, innerWidth)-2)
	right := ansi.Truncate(count, rightBudget, "")
	for _, chevron := range []string{contextBarChevronCollapsed, contextBarChevronExpanded} {
		if strings.HasSuffix(count, " "+chevron) && rightBudget > 0 {
			right = ansi.Truncate(strings.TrimSuffix(count, " "+chevron), max(0, rightBudget-2), "")
			if right != "" {
				right += " "
			}
			right += chevron
		}
	}
	right = styles.AttachmentSizeStyle.Render(right)
	leftBudget := max(0, innerWidth-ansi.StringWidth(right)-2)
	if right == "" {
		leftBudget = innerWidth
	}
	fullWidth := 0
	for i, item := range b.attachments {
		fullWidth += ansi.StringWidth(renderAttachmentPill(item))
		if i > 0 {
			fullWidth += 2
		}
	}
	overflowCue := len(b.attachments) > 1 && fullWidth > leftBudget && leftBudget >= 3
	if overflowCue {
		leftBudget = max(0, leftBudget-2)
	}
	var left string
	for i, item := range b.attachments {
		pill := b.renderPill(item)
		start := ansi.StringWidth(left)
		if i > 0 {
			start += 2
			if start+ansi.StringWidth(pill) > leftBudget {
				b.hidden = b.attachments[i:]
				break
			}
			left += "  "
		} else {
			pill = ansi.Truncate(pill, leftBudget, "…")
		}
		end := start + ansi.StringWidth(pill)
		if end > start {
			b.regions = append(b.regions, bannerRegion{start: start, end: end, y: b.summaryY(), item: item})
		}
		left += pill
	}
	if len(b.hidden) > 0 && overflowCue {
		left += " …"
	}
	return left, right
}

func (b *contextBar) ToggleAt(x, y int) bool {
	return b.canExpand && x >= 0 && x < b.width && y >= 0 && y < b.height
}

func renderAttachmentPill(item bannerItem) string { return renderHoveredAttachmentPill(item, 0) }

func renderHoveredAttachmentPill(item bannerItem, progress float64) string {
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(item.label))
	name, size := parseLabel(label)
	pill := styles.AttachmentIconStyle.Render("📎 ") + styles.HoverText(styles.AttachmentBadgeStyle.Render(name), progress, styles.TextPrimary)
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
