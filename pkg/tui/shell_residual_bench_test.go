package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// residualFixture preserves the production owner tick and View path. The replay
// harness records every message for assertions; unwrap it here so long benchmark
// runs do not retain an unbounded tick history. No provider or terminal commands
// execute, and the optional notification expiry command is intentionally omitted.
func residualFixture(tb testing.TB, scenario string, tabCount int) (*appModel, *paneReplayScheduler) {
	root, _, scheduler := paneReplayRoot(tb)
	for id, wrapped := range root.chatPages {
		switch page := wrapped.(type) {
		case *paneReplayPage:
			root.chatPages[id] = page.Page
		case *splitRecordingPage:
			root.chatPages[id] = page.Page
		}
	}
	root.chatPage = root.chatPages["profile"]
	root.singlePane()
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.editor.SetValue(strings.Repeat("work item 界 unicode words ", 80))
	root.resizeAll()
	if scenario != "idle" {
		root.Update(runtime.StreamStarted("profile", "root"))
		tabs, active := root.supervisor.GetTabs()
		if tabCount < len(tabs) {
			tabs = tabs[:tabCount]
		}
		for i := range tabs {
			if tabs[i].SessionID == "profile" {
				tabs[i].Activity = messages.TabActivityRunning
			}
		}
		for len(tabs) < tabCount {
			extra := tabs[0]
			extra.SessionID = fmt.Sprint("extra", len(tabs))
			tabs = append(tabs, extra)
		}
		root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	}
	if scenario == "notification" {
		root.Update(notification.ShowMsg{Text: "Stable notification above the draft"})
	}
	if scenario == "hidden" {
		root.handleSwitchTab("second")
		root.singlePane()
	}
	if scenario == "loading" {
		root.beginSubagentOpening("child-node", "target", "Worker", "root")
		tb.Cleanup(root.cancelSessionOpening)
	}
	for range 240 {
		if len(scheduler.pending) == 0 {
			break
		}
		residualTick(tb, root, scheduler)
		root.View()
	}
	root.View()
	return root, scheduler
}

func residualTick(tb testing.TB, root *appModel, s *paneReplayScheduler) {
	tb.Helper()
	if len(s.pending) == 0 {
		tb.Fatal("no owner lease")
	}
	cmd := s.pending[0]
	s.pending = s.pending[1:]
	root.Update(cmd())
}

var residualViewSink tea.View

// BenchmarkShellResidual isolates accepted active ticks, cached views, and
// artificial unrelated messages. "Clean" advances 1ns, not wall-clock idle;
// "Real" advances 16.666667ms without sleeping. Histories remain bounded.
// Every case uses one visible pane; Tabs1/12 vary tab-strip count, not panes.
// Run: go test ./pkg/tui -run '^$' -bench '^BenchmarkShellResidual$' -benchmem -benchtime=500ms -count=3
func BenchmarkShellResidual(b *testing.B) {
	for _, spec := range []struct {
		name, scenario, mode string
		step                 time.Duration
		tabs                 int
	}{
		{"CleanCombined", "active", "combined", time.Nanosecond, 3},
		{"CleanUpdate", "active", "update", time.Nanosecond, 3},
		{"CachedView", "active", "view", time.Nanosecond, 3},
		{"RealCombined", "active", "combined", 16666667 * time.Nanosecond, 3},
		{"ChangedCombined", "active", "combined", 120 * time.Millisecond, 3},
		{"RealUpdate", "active", "update", 16666667 * time.Nanosecond, 3},
		{"ForcedCompose", "active", "compose", time.Nanosecond, 3},
		{"IdleView", "idle", "view", time.Nanosecond, 3},
		{"IdleUnrelatedUpdateView", "idle", "idle", time.Nanosecond, 3},
		{"NotificationClean", "notification", "combined", time.Nanosecond, 3},
		{"NotificationReal", "notification", "combined", 16666667 * time.Nanosecond, 3},
		{"HiddenView", "hidden", "view", 16666667 * time.Nanosecond, 3},
		{"LoadingClean", "loading", "combined", time.Nanosecond, 3},
		{"LoadingReal", "loading", "combined", 16666667 * time.Nanosecond, 3},
		{"Tabs1Clean", "active", "combined", time.Nanosecond, 1},
		{"Tabs12Clean", "active", "combined", time.Nanosecond, 12},
	} {
		b.Run(spec.name, func(b *testing.B) {
			root, s := residualFixture(b, spec.scenario, spec.tabs)
			// Start clean measurements inside, rather than next to, a glyph
			// interval. The bound below prevents adaptive runs crossing it.
			if spec.step == time.Nanosecond && len(s.pending) > 0 {
				s.step = 5 * time.Millisecond
				residualTick(b, root, s)
				root.View()
			}
			s.step = spec.step
			start := root.ar.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				switch spec.mode {
				case "view":
					residualViewSink = root.View()
				case "compose":
					residualViewSink = root.composeView()
				case "idle":
					root.Update(struct{}{})
					residualViewSink = root.View()
				default:
					residualTick(b, root, s)
					if spec.mode != "update" {
						residualViewSink = root.View()
					}
				}
			}
			if spec.step == time.Nanosecond && (spec.mode == "combined" || spec.mode == "update") &&
				(root.ar.Now()-start >= time.Millisecond || animation.Chat.FrameIndexAt(start) != animation.Chat.FrameIndexAt(root.ar.Now()) || animation.Card.FrameIndexAt(start) != animation.Card.FrameIndexAt(root.ar.Now())) {
				b.Fatal("clean benchmark exceeded bounded sub-frame clock budget; reduce benchtime")
			}
		})
	}
}
