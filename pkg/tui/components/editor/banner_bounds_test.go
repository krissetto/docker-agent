package editor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestBannerBudgetBoundsLabelsRowsAndHitRegions(t *testing.T) {
	for _, expanded := range []bool{false, true} {
		for _, budget := range []int{0, 1, 2, 3, 4, 6} {
			for _, width := range []int{1, 2, 8, 24, 80} {
				t.Run(fmt.Sprintf("expanded=%v/budget=%d/width=%d", expanded, budget, width), func(t *testing.T) {
					b := newContextBar()
					var items []bannerItem
					for i := range 20 {
						items = append(items, bannerItem{label: fmt.Sprintf("界-file-%d\nsecond\r\trow (2 KB)", i), placeholder: fmt.Sprintf("@file-%d", i)})
					}
					b.SetItems(items)
					b.SetMaxHeight(budget)
					b.SetSize(width)
					if expanded {
						b.Toggle()
					}
					view := b.View(width)
					if budget == 0 {
						require.Empty(t, view)
						require.Zero(t, b.Height())
					} else {
						lines := strings.Split(view, "\n")
						require.Len(t, lines, b.Height())
						require.LessOrEqual(t, len(lines), budget)
						for _, line := range lines {
							require.LessOrEqual(t, ansi.StringWidth(line), width)
						}
					}
					for _, region := range b.regions {
						if region.end <= region.start {
							continue
						}
						require.Less(t, region.y, b.Height())
						item, ok := b.HitTestPosition(styles.AppPadding+region.start, region.y)
						require.True(t, ok)
						require.Equal(t, region.item, item)
					}
					_, ok := b.HitTestPosition(width, 0)
					require.False(t, ok)
					_, ok = b.HitTestPosition(styles.AppPadding, budget)
					require.False(t, ok, "omitted rows have no attachment target")
					require.Equal(t, items, b.attachments, "display normalization never changes source labels")
				})
			}
		}
	}
}

func TestBannerBudgetRetainsDraftAndInvalidatesOldRegions(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 4)
	draft := "prior message\nreply\nfollow-up  exact text"
	e.SetValue(draft)
	e.banner.SetItems([]bannerItem{{label: "paste-1 (2 KB)", placeholder: "@paste-1"}})
	e.BannerView(40)
	e.ToggleContextBar()
	_, ok := e.banner.HitTestPosition(styles.AppPadding, 2)
	require.True(t, ok)
	e.SetBannerMaxHeight(1)
	require.Equal(t, 1, e.BannerHeight())
	_, ok = e.banner.HitTestPosition(styles.AppPadding, 3)
	require.False(t, ok, "budget changes invalidate old geometry before rendering")
	e.SetContextBarFocused(true)
	e.SetBannerMaxHeight(0)
	require.False(t, e.IsContextBarFocused())
	require.Empty(t, e.BannerView(40))
	require.Equal(t, draft, e.Value())
	require.Len(t, e.banner.attachments, 1)
	e.SetBannerMaxHeight(4)
	require.Equal(t, 3, e.BannerHeight())
	e.BannerView(40)
	_, ok = e.banner.HitTestPosition(styles.AppPadding, 2)
	require.True(t, ok, "restored geometry recovers the same attachment")
}
