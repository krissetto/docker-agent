package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// BenchmarkShellPresentation uses the production owner Update/View path and
// accepted tick leases with bounded histories. Timer callbacks advance a
// deterministic clock instead of sleeping. Commands other than the owner tick
// are deliberately not executed (no provider, network, or terminal IO).
//
// Run on both revisions with this same file:
// go test ./pkg/tui -run '^$' -bench '^BenchmarkShellPresentation$' -benchmem -benchtime=500ms -count=3
func BenchmarkShellPresentation(b *testing.B) {
	for _, panes := range []int{1, 3} {
		for _, draft := range []string{"empty", "long"} {
			for _, changed := range []bool{false, true} {
				name := fmt.Sprintf("panes=%d/draft=%s/changed=%t", panes, draft, changed)
				b.Run(name, func(b *testing.B) { benchmarkShellPresentation(b, panes, draft, changed, false) })
			}
		}
	}
	for _, changed := range []bool{false, true} {
		b.Run(fmt.Sprintf("panes=3/draft=long/changed=%t/notification", changed), func(b *testing.B) { benchmarkShellPresentation(b, 3, "long", changed, true) })
	}
}

func benchmarkShellPresentation(b *testing.B, panes int, draft string, changed, toast bool) {
	root, _, scheduler := paneReplayRoot(b)
	if panes == 1 {
		root.singlePane()
	}
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	if draft == "long" {
		root.editor.SetValue(strings.Repeat("work item 界 unicode words ", 80))
	}
	root.resizeAll()
	root.Update(runtime.StreamStarted("profile", "root"))
	tabs, active := root.supervisor.GetTabs()
	for i := range tabs {
		if tabs[i].SessionID == "profile" {
			tabs[i].Activity = messages.TabActivityRunning
		}
	}
	root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	if toast {
		root.Update(notification.ShowMsg{Text: "Stable notification above the draft"})
	}
	// Settle finite presentation transitions; StreamStarted keeps a real spinner
	// registered. Drain each timer through Update so ownership remains canonical.
	for range 240 {
		if len(scheduler.pending) == 0 {
			b.Fatal("working fixture lost animation lease")
		}
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		root.Update(cmd())
		root.View()
	}
	root.View()
	// 1ns preserves the warmed glyph (clean tick), while 120ms advances chat
	// and card frames (changed tick). Both still use genuine owner leases.
	scheduler.step = time.Nanosecond
	if changed {
		scheduler.step = 120 * time.Millisecond
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if len(scheduler.pending) == 0 {
			b.Fatal("working fixture lost tick")
		}
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		root.Update(cmd())
		root.View()
	}
}
