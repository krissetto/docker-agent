package editor

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type bannerScheduler struct{ now time.Time }

func (s *bannerScheduler) Now() time.Time { return s.now }
func (s *bannerScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return f(s.now) }
}
func hoverBannerFixture(t *testing.T, labels ...string) (*editor, *animation.Runtime) {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&bannerScheduler{now: time.Unix(1, 0)})
	e := New(nil, WithAnimationRuntime(ar)).(*editor)
	e.SetBannerWidth(40)
	e.banner.SetItems(bannerItems(labels...))
	t.Cleanup(e.Cleanup)
	return e, ar
}
func settleBanner(t *testing.T, e *editor, ar *animation.Runtime) {
	t.Helper()
	for range 30 {
		cmd := ar.Continue()
		if cmd == nil {
			require.Zero(t, ar.ActiveCount())
			return
		}
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		e.TickBannerHover(tick)
	}
	t.Fatal("banner hover retained an idle lease")
}
func TestBannerHoverUsesSharedCadenceAndOnlyNameAndCountCells(t *testing.T) {
	e, ar := hoverBannerFixture(t, "first (2 KB)", "界-é.txt", "third")
	b := e.banner
	rest := uv.NewStyledString(b.View(40)).Lines(ansi.GraphemeWidth)
	e.HoverBanner(styles.AppPadding, b.summaryY())
	require.EqualValues(t, 1, ar.ActiveCount(), "count and name share one subscription")
	tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
	require.True(t, ok)
	before, after := tick.ElapsedBounds()
	e.TickBannerHover(tick)
	require.Equal(t, animation.HoverStep(0, 1, after-before), b.hoverValues[b.attachments[0].placeholder].value)
	settleBanner(t, e, ar)
	require.Equal(t, 1.0, b.hoverValues["count"].value)
	require.Equal(t, 1.0, b.hoverValues[b.attachments[0].placeholder].value)
	hovered := uv.NewStyledString(b.View(40)).Lines(ansi.GraphemeWidth)
	changes := 0
	for y, row := range hovered {
		require.Len(t, row, len(rest[y]))
		x := 0
		for i, cell := range row {
			require.Nil(t, cell.Style.Bg)
			if cell != rest[y][i] {
				changes++
				require.Equal(t, b.summaryY(), y)
				count := x >= b.toggleRegion.start+styles.AppPadding && x < b.toggleRegion.end+styles.AppPadding
				name := x >= styles.AppPadding+3 && x < styles.AppPadding+3+len("first")
				require.True(t, count || name, "only name/count foreground changes at %d,%d", x, y)
			}
			x += cell.Width
		}
	}
	require.Positive(t, changes)
	e.HoverBanner(styles.AppPadding, b.summaryY())
	require.Zero(t, ar.ActiveCount(), "settled repeat motion stays idle")
	e.HoverBanner(-1, -1)
	settleBanner(t, e, ar)
	require.Empty(t, b.hoverValues)
	require.Equal(t, rest, uv.NewStyledString(b.View(40)).Lines(ansi.GraphemeWidth))
}
func TestBannerHoverAllFitAndGeometryCancellation(t *testing.T) {
	e, ar := hoverBannerFixture(t, "only.txt")
	e.HoverBanner(0, 0)
	require.Zero(t, ar.ActiveCount(), "all-fit count is not actionable")
	e.HoverBanner(styles.AppPadding, 2)
	settleBanner(t, e, ar)
	require.Equal(t, 1.0, e.banner.hoverValues[e.banner.attachments[0].placeholder].value)
	require.Zero(t, e.banner.hoverValues["count"].value)
	for _, mutate := range []func(){func() { e.SetBannerWidth(30) }, func() { e.SetBannerMaxHeight(2) }, func() { e.banner.SetItems(bannerItems("different")) }, func() { e.banner.SetItems(nil) }, func() { e.CancelBannerHover() }} {
		e.SetBannerMaxHeight(6)
		e.SetBannerWidth(40)
		e.banner.SetItems(bannerItems("one", "two", "three"))
		e.HoverBanner(styles.AppPadding, 2)
		require.EqualValues(t, 1, ar.ActiveCount())
		mutate()
		require.Zero(t, ar.ActiveCount())
		require.Empty(t, e.banner.hoverValues)
	}
}
func TestBannerOverflowAlwaysShowsTrailingEllipsis(t *testing.T) {
	for _, width := range []int{12, 20, 30, 40, 60} {
		e, _ := hoverBannerFixture(t, "界-é.txt", "long-other-file.txt", "last.txt")
		e.SetBannerWidth(width)
		b := e.banner
		require.NotEmpty(t, b.hidden)
		require.Contains(t, ansi.Strip(b.View(width)), "…")
		for _, line := range uv.NewStyledString(b.View(width)).Lines(ansi.GraphemeWidth) {
			cells := 0
			for _, cell := range line {
				cells += cell.Width
			}
			require.LessOrEqual(t, cells, width)
		}
	}
}
