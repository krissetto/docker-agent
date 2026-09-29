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
	return residualPaneFixture(tb, scenario, tabCount, 1, "long")
}

func residualPaneFixture(tb testing.TB, scenario string, tabCount, panes int, draft string) (*appModel, *paneReplayScheduler) {
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
	if panes == 1 {
		root.singlePane()
	}
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	if draft == "long" {
		root.editor.SetValue(strings.Repeat("work item 界 unicode words ", 80))
	} else {
		root.editor.SetValue("")
	}
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
// Legacy named cases use one visible pane; Tabs1/12 vary tab-strip count.
// Matrix explicitly varies visible panes and draft occupancy.
// Run: go test ./pkg/tui -run '^$' -bench '^BenchmarkShellResidual$' -benchmem -benchtime=500ms -count=3
func BenchmarkShellResidual(b *testing.B) {
	b.Run("Matrix", func(b *testing.B) {
		for _, panes := range []int{1, 3} {
			for _, draft := range []string{"empty", "long"} {
				for _, cadence := range []string{"clean", "changed", "real"} {
					for _, toast := range []bool{false, true} {
						name := fmt.Sprintf("panes=%d/draft=%s/%s/toast=%t", panes, draft, cadence, toast)
						b.Run(name, func(b *testing.B) {
							root, scheduler := residualMatrixFixture(b, panes, draft, cadence, toast)
							start := root.ar.Now()
							b.ReportAllocs()
							b.ResetTimer()
							for b.Loop() {
								residualTick(b, root, scheduler)
								residualViewSink = root.View()
							}
							b.StopTimer()
							if cadence == "clean" && (root.ar.Now()-start >= time.Millisecond || animation.Chat.FrameIndexAt(start) != animation.Chat.FrameIndexAt(root.ar.Now()) || animation.Card.FrameIndexAt(start) != animation.Card.FrameIndexAt(root.ar.Now())) {
								b.Fatal("clean matrix exceeded sub-frame budget; reduce benchtime")
							}
						})
					}
				}
			}
		}
	})

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

// Validate changed *output*, not merely elapsed time or a dirty bit. Validation
// stays outside benchmark timers; real cadence intentionally mixes equal frames.
func residualMatrixFixture(tb testing.TB, panes int, draft, cadence string, toast bool) (*appModel, *paneReplayScheduler) {
	scenario := "active"
	if toast {
		scenario = "notification"
	}
	root, scheduler := residualPaneFixture(tb, scenario, 3, panes, draft)
	scheduler.step = 5 * time.Millisecond
	residualTick(tb, root, scheduler)
	before := root.View().Content
	switch cadence {
	case "clean":
		scheduler.step = time.Nanosecond
	case "changed":
		scheduler.step = 120 * time.Millisecond
	case "real":
		scheduler.step = 16666667 * time.Nanosecond
	default:
		tb.Fatalf("unknown cadence %q", cadence)
	}
	residualTick(tb, root, scheduler)
	after := root.View().Content
	if cadence == "clean" && before != after {
		tb.Fatal("clean fixture changed output")
	}
	if cadence == "changed" && before == after {
		tb.Fatal("changed fixture did not change output")
	}
	if len(root.panes.Sessions()) != panes {
		tb.Fatalf("got %d panes, want %d", len(root.panes.Sessions()), panes)
	}
	return root, scheduler
}

// Each operation uses root Update/View, not editor-only or page-only methods.
// Typing is reversible and scroll oscillates locally; streaming is deliberately
// finite (32 chunks), never an adaptively growing benchmark history.
func residualInput(root *appModel, kind string, step int) {
	switch kind {
	case "typing":
		key := tea.KeyPressMsg{Code: 'x', Text: "x"}
		if step%2 != 0 {
			key = tea.KeyPressMsg{Code: tea.KeyBackspace}
		}
		root.Update(key)
	case "scroll":
		key := tea.KeyPressMsg{Code: tea.KeyPgUp}
		if step%2 != 0 {
			key.Code = tea.KeyPgDown
		}
		root.Update(key)
	case "stream":
		generation, _ := root.supervisor.RouteGeneration("profile")
		root.Update(messages.RoutedMsg{SessionID: "profile", RouteGeneration: generation, Inner: runtime.AgentChoice("root", "profile", " bounded stream λ界 `code` ")})
	}
	residualViewSink = root.View()
}

func residualInputFixture(tb testing.TB, panes int, kind string) *appModel {
	draft := "long"
	if kind == "typing" {
		draft = "empty"
	}
	root, _ := residualPaneFixture(tb, "active", 3, panes, draft)
	if kind == "scroll" {
		root.focusedPanel = PanelContent
		root.chatPage.FocusMessages()
		root.chatPage.ScrollToBottom()
		root.viewCacheValid = false
	}
	root.View()
	return root
}

// Run separately: -bench '^BenchmarkShellResidualInputs$' -benchtime=1x -count=5.
// ns/op and allocations describe a whole 32-event replay, not a single event.
func BenchmarkShellResidualInputs(b *testing.B) {
	for _, panes := range []int{1, 3} {
		for _, kind := range []string{"typing", "scroll", "stream"} {
			b.Run(fmt.Sprintf("panes=%d/%s", panes, kind), func(b *testing.B) {
				if b.N != 1 {
					b.Fatal("bounded input replay requires -benchtime=1x")
				}
				b.StopTimer()
				root := residualInputFixture(b, panes, kind)
				b.ReportAllocs()
				b.StartTimer()
				for step := range 32 {
					residualInput(root, kind, step)
				}
				b.StopTimer()
				b.ReportMetric(32, "events/replay")
			})
		}
	}
}

func TestResidualMatrixFixtures(t *testing.T) {
	for _, panes := range []int{1, 3} {
		for _, draft := range []string{"empty", "long"} {
			for _, cadence := range []string{"clean", "changed", "real"} {
				for _, toast := range []bool{false, true} {
					t.Run(fmt.Sprintf("panes=%d/draft=%s/%s/toast=%t", panes, draft, cadence, toast), func(t *testing.T) {
						root, scheduler := residualMatrixFixture(t, panes, draft, cadence, toast)
						before := root.View().Content
						changed := 0
						for range 24 {
							residualTick(t, root, scheduler)
							after := root.View().Content
							if before != after {
								changed++
							}
							before = after
						}
						if cadence == "clean" && changed != 0 || cadence == "changed" && changed != 24 || cadence == "real" && (changed == 0 || changed == 24) {
							t.Fatalf("cadence=%s changed=%d/24", cadence, changed)
						}
						t.Logf("changed=%d/24 pending=%d", changed, len(scheduler.pending))
					})
				}
			}
		}
		for _, kind := range []string{"typing", "scroll", "stream"} {
			t.Run(fmt.Sprintf("panes=%d/%s", panes, kind), func(t *testing.T) {
				root := residualInputFixture(t, panes, kind)
				value := root.editor.Value()
				before := root.View().Content
				changed := 0
				for step := range 32 {
					residualInput(root, kind, step)
					after := residualViewSink.Content
					if before != after {
						changed++
					}
					before = after
				}
				if changed == 0 {
					t.Fatal("input replay never changed output")
				}
				if kind == "typing" && value != root.editor.Value() {
					t.Fatalf("typing replay changed draft: before=%d after=%d suffix=%q", len(value), len(root.editor.Value()), root.editor.Value()[max(0, len(root.editor.Value())-80):])
				}
				t.Logf("changed=%d/32", changed)
			})
		}
	}
}
