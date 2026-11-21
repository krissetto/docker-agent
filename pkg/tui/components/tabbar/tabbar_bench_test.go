package tabbar

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func benchmarkTabBar() *TabBar {
	bar := New(animation.NewRuntime(), 0)
	bar.SetWidth(100)
	bar.SetTabs([]messages.TabInfo{
		{SessionID: "a", Title: "Main session", IsActive: true},
		{SessionID: "b", Title: "Investigation"},
		{SessionID: "c", Title: "Implementation"},
		{SessionID: "d", Title: "Review"},
	}, 0)
	bar.View()
	return bar
}

func BenchmarkTabBar(b *testing.B) {
	b.Run("ViewIdle", func(b *testing.B) {
		bar := benchmarkTabBar()
		for b.Loop() {
			bar.View()
		}
	})
	b.Run("ViewDragging", func(b *testing.B) {
		bar := benchmarkTabBar()
		bar.handleLeftClickDown(5)
		bar.handleMouseMotion(38)
		for b.Loop() {
			bar.View()
		}
	})
	b.Run("DragMotion", func(b *testing.B) {
		bar := benchmarkTabBar()
		bar.handleLeftClickDown(5)
		x := 38
		for b.Loop() {
			bar.handleMouseMotion(x)
			x = 95 - x
		}
	})
}
