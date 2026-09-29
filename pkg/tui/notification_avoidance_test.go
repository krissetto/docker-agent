package tui

import (
	"image"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRootNotificationAvoidanceBothCompositorsAndHitGeometry(t *testing.T) {
	for _, lean := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "lean"}[lean], func(t *testing.T) {
			clock := &rootImmediateScheduler{now: time.Unix(1, 0)}
			root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(clock))
			root.leanMode = lean
			root.panelSettings = messages.PanelSettings{Elements: []messages.PanelElement{}}
			root.resizeAll()
			root.Update(notification.ShowMsg{Text: "Notification avoids typed input"})
			resting := root.notification.GetLayer().GetY()
			root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
			root.focusedPanel = PanelEditor
			root.editor.Focus()
			root.resizeAll()
			root.syncNotificationAvoidance()
			require.True(t, root.ar.HasActive(), "overlap registers shared motion")
			for range 80 {
				cmd := root.ar.Continue()
				if cmd == nil {
					break
				}
				root.updateWithLifecycle(cmd())
				root.syncNotificationAvoidance()
			}
			layer := root.notification.GetLayer()
			require.Less(t, layer.GetY(), resting)
			frame := root.editorFrame()
			origin := image.Pt(frame.GetMarginLeft()+frame.GetBorderLeftSize()+frame.GetPaddingLeft(), root.editorTop()+frame.GetMarginTop()+frame.GetBorderTopSize()+frame.GetPaddingTop())
			bounds := image.Rect(layer.GetX(), layer.GetY(), layer.GetX()+layer.Width(), layer.GetY()+layer.Height())
			for _, cell := range root.editor.(editor.TextOccupancy).OccupiedTextCells() {
				require.False(t, bounds.Overlaps(cell.Add(origin)), "shifted overlay clears visible Unicode input")
			}
			require.Contains(t, root.View().Content, "Notification avoids typed input")
			root.notification.HandleMouseMotion(layer.GetX()+2, layer.GetY()+1)
			_, _, hit := root.notification.CopyHit(layer.GetX()+2, layer.GetY()+1)
			require.True(t, hit, "copy geometry moves with the painted stack")
			root.editor.SetValue("")
			root.syncNotificationAvoidance()
			for range 80 {
				cmd := root.ar.Continue()
				if cmd == nil {
					break
				}
				root.updateWithLifecycle(cmd())
				root.syncNotificationAvoidance()
			}
			require.Equal(t, resting, root.notification.GetLayer().GetY())
			require.False(t, root.ar.HasActive(), "settled empty editor leaves no avoidance tick")
			root.notification.Cleanup()
		})
	}
}

func TestRootNotificationAvoidanceIgnoresLoadingEmptyAndUnfocused(t *testing.T) {
	for _, mode := range []string{"loading", "empty", "unfocused", "zero-size"} {
		t.Run(mode, func(t *testing.T) {
			root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)}))
			root.notification.Update(notification.ShowMsg{Text: "No movement without visible focused text"})
			root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
			root.editor.Focus()
			root.resizeAll()
			root.focusedPanel = PanelEditor
			switch mode {
			case "loading":
				root.ready = false
			case "empty":
				root.editor.SetValue("")
			case "unfocused":
				root.focusedPanel = PanelContent
				root.editor.Blur()
			case "zero-size":
				root.width = 0
				root.height = 0
			}
			resting := root.notification.GetLayer().GetY()
			root.syncNotificationAvoidance()
			require.Equal(t, resting, root.notification.GetLayer().GetY())
			require.False(t, root.ar.HasActive(), "invisible occupancy cannot register motion")
			root.notification.Cleanup()
		})
	}
}

type countedOccupancyEditor struct {
	editor.Editor
	calls int
}

func (e *countedOccupancyEditor) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	model, cmd := e.Editor.Update(msg)
	e.Editor = model.(editor.Editor)
	return e, cmd
}

func (e *countedOccupancyEditor) OccupiedTextCells() []image.Rectangle {
	e.calls++
	return e.Editor.(editor.TextOccupancy).OccupiedTextCells()
}

func TestRootNotificationAvoidanceSkipsClosedAndRefreshesOnShow(t *testing.T) {
	root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)}))
	root.panelSettings = messages.PanelSettings{Elements: []messages.PanelElement{}}
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.editor.SetValue(strings.Repeat("界", 42) + "\n" + strings.Repeat("字", 42))
	root.resizeAll()
	counted := &countedOccupancyEditor{Editor: root.editor}
	root.editor = counted
	for range 3 {
		root.Update(struct{}{})
	}
	require.Zero(t, counted.calls, "closed notifications must not query occupancy")
	root.Update(notification.ShowMsg{Text: "Notification avoids current input"})
	require.Positive(t, counted.calls, "Show's own Update epilogue must query current occupancy")
	require.True(t, root.ar.HasActive(), "first shown frame schedules avoidance without another input event")
	root.Update(notification.HideMsg{})
	calls := counted.calls
	root.editor.SetValue("")
	root.Update(struct{}{})
	require.Equal(t, calls, counted.calls)
	root.Update(notification.ShowMsg{Text: "Reopened over empty input"})
	require.Greater(t, counted.calls, calls)
	require.False(t, root.ar.HasActive(), "reopening cannot reuse the previous notification's occupancy")
	root.notification.Cleanup()
}

func TestRootNotificationAvoidanceStableStorageAndLegacyFallback(t *testing.T) {
	root, _, _ := harnessRoot(t, 156, 48, animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)}))
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.editor.SetValue(strings.Repeat("work item 界 unicode words ", 80))
	root.resizeAll()
	root.Update(notification.ShowMsg{Text: "Stable notification above draft"})
	root.syncNotificationAvoidance()
	require.NotEmpty(t, root.notificationOccupied)
	require.Zero(t, testing.AllocsPerRun(20, root.syncNotificationAvoidance), "warm root sync reuses value snapshot and caller-owned coordinates")
	want := append([]image.Rectangle(nil), root.notificationOccupied...)
	// Optional append support must not be required of existing Editor wrappers.
	legacy := &countedOccupancyEditor{Editor: root.editor}
	root.editor = legacy
	root.syncNotificationAvoidance()
	require.Equal(t, 1, legacy.calls)
	require.Equal(t, want, root.notificationOccupied)
	root.notification.Cleanup()
}

func TestRootNotificationAvoidanceTransitionsMatchCompleteFreshView(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	root, _, scheduler := paneReplayRoot(t)
	root.singlePane()
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.editor.SetValue(strings.Repeat("界", 70) + "\n" + strings.Repeat("字", 70))
	root.resizeAll()
	check := func() {
		t.Helper()
		cached := root.View()
		root.viewCacheValid = false
		require.Equal(t, root.View(), cached, "full tea.View parity includes overlay and terminal metadata")
	}
	for _, step := range []struct {
		name string
		run  func()
	}{
		{"show", func() { root.Update(notification.ShowMsg{Text: "Notification transition parity"}) }},
		{"edit", func() { root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}) }},
		{"paste", func() { root.Update(tea.PasteMsg{Content: " pasted 界"}) }},
		{"resize", func() { root.Update(tea.WindowSizeMsg{Width: 130, Height: 42}) }},
		{"theme", func() {
			theme := *original
			theme.Colors.TextMuted = "#123456"
			styles.ApplyTheme(&theme)
			root.Update(messages.ThemeChangedMsg{})
		}},
		{"blur", func() { root.editor.Blur(); root.focusedPanel = PanelContent; root.Update(struct{}{}) }},
		{"focus", func() { root.editor.Focus(); root.focusedPanel = PanelEditor; root.Update(struct{}{}) }},
		{"clear", func() { root.editor.SetValue(""); root.Update(struct{}{}) }},
		{"hide", func() { root.Update(notification.HideMsg{}) }},
		{"reopen", func() {
			root.editor.SetValue(strings.Repeat("new text ", 30))
			root.resizeAll()
			root.Update(notification.ShowMsg{Text: "Reopened notification"})
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.run()
			check()
			for i := 0; i < 80 && len(scheduler.pending) > 0; i++ {
				cmd := scheduler.pending[0]
				scheduler.pending = scheduler.pending[1:]
				root.Update(cmd())
				check()
			}
		})
	}
	root.notification.Cleanup()
}
