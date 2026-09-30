package editor

import (
	"fmt"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func bannerItems(labels ...string) []bannerItem {
	items := make([]bannerItem, len(labels))
	for i, label := range labels {
		items[i] = bannerItem{label: label, placeholder: fmt.Sprintf("@dir-%d/%s", i, label)}
	}
	return items
}

func TestBannerAllFitHasNoExpansion(t *testing.T) {
	for _, labels := range [][]string{nil, {"file"}, {strings.Repeat("long界", 80)}, {"a", "b", "c"}} {
		b := newContextBar()
		b.SetSize(80)
		b.SetItems(bannerItems(labels...))
		before, height := b.View(80), b.Height()
		b.Toggle()
		require.False(t, b.IsExpanded())
		require.Equal(t, height, b.Height())
		require.Equal(t, before, b.View(80))
		require.Empty(t, b.hidden)
		require.NotContains(t, ansi.Strip(before), contextBarChevronCollapsed)
		if len(labels) > 0 {
			require.Contains(t, ansi.Strip(before), fmt.Sprintf("%d attachment", len(labels)))
			require.Len(t, b.regions, len(labels))
		}
		for y := range height {
			for x := range 80 {
				require.False(t, b.ToggleAt(x, y))
			}
		}
	}
}

func TestBannerExpansionOnlyRevealsHiddenIdentities(t *testing.T) {
	b := newContextBar()
	items := bannerItems("界-e\u0301.txt", "same.txt", "same.txt", "last.txt")
	b.SetSize(40)
	b.SetItems(items)
	require.True(t, b.canExpand)
	require.NotEmpty(t, b.hidden)
	collapsed := append([]bannerRegion(nil), b.regions...)
	hidden := append([]bannerItem(nil), b.hidden...)
	summary := strings.Split(ansi.Strip(b.View(40)), "\n")[b.summaryY()]
	b.Toggle()
	require.True(t, b.IsExpanded())
	require.Equal(t, collapsed, b.regions[:len(collapsed)])
	require.Equal(t, strings.Replace(summary, contextBarChevronCollapsed, contextBarChevronExpanded, 1), strings.Split(ansi.Strip(b.View(40)), "\n")[b.summaryY()])
	for i, region := range b.regions[len(collapsed):] {
		require.Equal(t, hidden[i], region.item)
		got, ok := b.HitTestPosition(styles.AppPadding+region.start, region.y)
		require.True(t, ok)
		require.Equal(t, hidden[i].placeholder, got.placeholder)
	}
	require.Equal(t, min(3+len(hidden), b.maxHeight), b.Height())
	for y := range b.Height() {
		for x := range b.width {
			require.True(t, b.ToggleAt(x, y), "whole banner toggles meaningful overflow")
		}
	}
	b.SetSize(160)
	require.False(t, b.IsExpanded())
	require.Empty(t, b.hidden)
	require.Equal(t, 3, b.Height())
	b.SetSize(40)
	b.Toggle()
	b.SetItems(items[:1])
	require.False(t, b.IsExpanded())
	require.Empty(t, b.hidden)
	require.Equal(t, 3, b.Height())
	b.SetItems(bannerItems(strings.Repeat("界", 90), "new"))
	require.True(t, b.canExpand)
	require.Len(t, b.hidden, 1)
}

func TestBannerExpansionRequiresAdditionalRowBudget(t *testing.T) {
	for _, budget := range []int{0, 1, 2, 3, 4, 5, 6} {
		b := newContextBar()
		b.SetSize(30)
		b.SetMaxHeight(budget)
		b.SetItems(bannerItems("one", "two", "three", "four"))
		b.Toggle()
		require.Equal(t, budget > 3, b.IsExpanded())
		require.LessOrEqual(t, b.Height(), budget)
		if budget <= 3 {
			require.NotContains(t, ansi.Strip(b.View(30)), contextBarChevronCollapsed)
		}
		b.SetMaxHeight(3)
		require.False(t, b.IsExpanded())
		require.False(t, b.canExpand)
	}
}

func TestBannerFocusLeavesPaddingAndEmptyRowsTransparent(t *testing.T) {
	b := newContextBar()
	b.SetSize(40)
	b.SetItems(bannerItems("first", "second", "third", "fourth"))
	b.Toggle()
	before := uv.NewStyledString(b.View(40)).Lines(ansi.GraphemeWidth)
	b.SetFocused(true)
	after := uv.NewStyledString(b.View(40)).Lines(ansi.GraphemeWidth)
	require.Equal(t, len(before), len(after))
	changed := 0
	for y, row := range after {
		require.Len(t, row, len(before[y]), "row %d view %q", y, b.View(40))
		x := 0
		for i, cell := range row {
			require.Nil(t, cell.Style.Bg, "default terminal background at %d,%d", x, y)
			if cell != before[y][i] {
				changed++
				r := b.toggleRegion
				require.True(t, y == r.y && x >= r.start+styles.AppPadding && x < r.end+styles.AppPadding, "focus cue must stay on overflow label: %d,%d", x, y)
			}
			x += cell.Width
		}
	}
	require.Positive(t, changed, "focused overflow label retains an explicit cue")
}
