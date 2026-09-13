package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/help"
	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type shellSnapshot struct {
	editorHeight, editorTarget int
	heightMoving               bool
	ticks, compositions        int
	tickTimes                  []time.Time
	overlayTimes               []time.Time

	open, closing, paused                                                 bool
	resizeDragging                                                        bool
	focus                                                                 FocusedPanel
	active                                                                int32
	editor                                                                string
	contextView                                                           string
	content                                                               string
	tabView                                                               string
	tabY, tabHeight, overlayX, holdMessages, delayMessages, overlayFrames int
	overlay                                                               bool
	activeID                                                              string
	tabIDs                                                                []string
}

type (
	shellSnapshotMsg  struct{ reply chan shellSnapshot }
	shellProgramModel struct {
		root                                       *appModel
		ticks, compositions                        int
		tickTimes, overlayTimes                    []time.Time
		holdMessages, delayMessages, overlayFrames int
		lastOverlayX                               int
		hadOverlay                                 bool
	}
)

func (m *shellProgramModel) Init() tea.Cmd { return nil }
func (m *shellProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if q, ok := msg.(shellSnapshotMsg); ok {
		s := shellSnapshot{
			editorHeight: m.root.editorHeight, editorTarget: m.root.editorHeightTarget, heightMoving: m.root.editorHeightMotion.Running(), ticks: m.ticks, compositions: m.compositions,
			tickTimes: append([]time.Time(nil), m.tickTimes...), overlayTimes: append([]time.Time(nil), m.overlayTimes...),
			resizeDragging: m.root.isDragging, focus: m.root.focusedPanel, open: m.root.dialogMgr.Open(), closing: m.root.dialogMgr.Closing(), paused: m.root.tickPaused, active: m.root.ar.ActiveCount(), editor: m.root.editor.Value(), content: m.root.View().Content,
			tabView: m.root.tabBar.View(), tabY: m.root.contentHeight + 1, tabHeight: m.root.tabBar.Height(), holdMessages: m.holdMessages, delayMessages: m.delayMessages, overlayFrames: m.overlayFrames,
		}
		if m.root.contextBar != nil {
			s.contextView = m.root.contextBar.View()
		}
		if layer := m.root.tabBar.GetDragLayerInfo(tabFrameWidth(m.root.width), s.tabY); layer != nil {
			s.overlay = true
			s.overlayX = layer.X
		}
		if m.root.supervisor != nil {
			s.activeID = m.root.supervisor.ActiveID()
			tabs, _ := m.root.supervisor.GetTabs()
			for _, tab := range tabs {
				s.tabIDs = append(s.tabIDs, tab.SessionID)
			}
		}
		q.reply <- s
		return m, nil
	}
	switch msg.(type) {
	case tabbar.DragHoldMsg:
		m.holdMessages++
	case tabbar.ScrollDelayMsg:
		m.delayMessages++
	}
	before := m.root.ar.Now()
	_, cmd := m.root.Update(msg)
	if _, tick := msg.(animation.TickMsg); tick && m.root.ar.Now() != before {
		m.ticks++
		m.tickTimes = append(m.tickTimes, time.Now())
	}
	return m, cmd
}

func (m *shellProgramModel) View() tea.View {
	if !m.root.viewCacheValid {
		m.compositions++
	}
	view := m.root.View()
	layer := m.root.tabBar.GetDragLayerInfo(tabFrameWidth(m.root.width), m.root.contentHeight+1)
	if layer != nil {
		if !m.hadOverlay || layer.X != m.lastOverlayX {
			m.overlayFrames++
			m.overlayTimes = append(m.overlayTimes, time.Now())
		}
		m.lastOverlayX = layer.X
	}
	m.hadOverlay = layer != nil
	return view
}

func TestActualProgramDialogFadeInputAndIdleQuiescence(t *testing.T) {
	root := populateScrollableRoot(t)
	root.focusedPanel = PanelEditor
	writer := &cacheProgramWriter{}
	coalescer := tuiinput.NewMouseCoalescer()
	t.Cleanup(coalescer.Stop)
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(writer), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return coalescer.Filter(msg) }))
	coalescer.SetSender(program.Send)
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("program snapshot timed out")
			return shellSnapshot{}
		}
	}
	require.Eventually(t, func() bool { return writer.snapshot() != "" }, time.Second, time.Millisecond)
	program.Send(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(help.Document{})})
	opened := snapshot()
	require.True(t, opened.open)
	require.Positive(t, opened.active, "opening must use shared animation runtime")
	require.Eventually(t, func() bool { s := snapshot(); return s.open && s.active == 0 }, 2*time.Second, 5*time.Millisecond)
	beforeClose := snapshot()
	program.Send(dialog.CloseDialogMsg{})
	program.Send(tea.MouseClickMsg{X: 40, Y: beforeClose.tabY - 1, Button: tea.MouseLeft})
	program.Send(tea.PasteMsg{Content: "MUST-NOT-REACH-EDITOR"})
	closing := snapshot()
	require.True(t, closing.open, "closing layer remains modal")
	require.True(t, closing.closing)
	require.NotContains(t, closing.editor, "MUST-NOT")
	require.False(t, closing.resizeDragging, "closing layer blocks clicks on the underlying resize handle")
	require.Equal(t, PanelEditor, closing.focus, "modal closing preserves the editor focus owner")
	require.Eventually(t, func() bool { s := snapshot(); return !s.open && s.active == 0 }, 2*time.Second, 5*time.Millisecond)
	// Let the terminal flush the final frame, then observe a whole quiet window.
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled while waiting for the terminal flush")
	}
	settled := len(writer.snapshot())
	require.Never(t, func() bool { return len(writer.snapshot()) != settled }, 80*time.Millisecond, time.Millisecond,
		"settled dialogs must not emit terminal writes")
	program.Send(tea.PasteMsg{Content: "IMMEDIATE-IDLE-PASTE"})
	ready := snapshot()
	require.Equal(t, PanelEditor, ready.focus)
	require.Equal(t, "IMMEDIATE-IDLE-PASTE", ready.editor, "one paste is accepted exactly once after the close")
	require.Eventually(t, func() bool { return strings.Contains(writer.snapshot(), "IMMEDIATE-IDLE-PASTE") }, time.Second, time.Millisecond)
	before := snapshot().content
	program.Send(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelUp})
	program.Send(tea.MouseReleaseMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	require.NotEqual(t, before, snapshot().content, "release flushes effective wheel before boundary")
	program.Send(tea.BlurMsg{})
	require.True(t, snapshot().paused)
	program.Send(tea.FocusMsg{})
	require.False(t, snapshot().paused)
	program.Send(tea.WindowSizeMsg{Width: 83, Height: 29})
	require.Equal(t, "IMMEDIATE-IDLE-PASTE", snapshot().editor, "resize preserves editor draft")
}

func TestPointerBoundaryNormalizesBeforeClickWithoutEditorHistory(t *testing.T) {
	root := populateScrollableRoot(t)
	defer root.ar.Stop()
	before := root.View().Content
	_, _ = root.Update(messages.PointerBoundaryMsg{
		Pending: messages.PointerUpdateMsg{X: 1, Y: 1, HasWheel: true, WheelDelta: -2},
		Event:   tea.MouseReleaseMsg{X: 1, Y: 1, Button: tea.MouseLeft},
	})
	require.NotEqual(t, before, root.View().Content)
	require.Empty(t, root.editor.Value())
	require.Zero(t, root.chatPage.QueueLength())
}

func TestClosingDialogSuppressesQuitWithoutOpeningAnotherDialog(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	defer root.dialogMgr.Cleanup()
	_, _ = root.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(help.Document{})})
	_, _ = root.Update(dialog.CloseDialogMsg{})
	require.True(t, root.dialogMgr.Closing())
	_, cmd := root.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.Nil(t, cmd, "closing is modal even for repeated quit")
	require.False(t, root.dialogMgr.TopIsExitConfirmation())
	root.transcriber = &fakeTranscriber{running: true}
	_, cmd = root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd, "closing blocks transcriber send path")
	require.True(t, root.transcriber.IsRunning())
}

func TestEditorExternalBannerGeometryAcrossResize(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	root.editor.SetValue(strings.Repeat("wrapped draft ", 9))
	_, _ = root.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	_, height := root.editor.GetSize()
	require.Greater(t, height, 3, "narrow width must remeasure wrapped content before layout")
	require.Equal(t, regionEditor, root.hitTestRegion(root.editorTop()))
	require.Equal(t, regionContextUsage, root.hitTestRegion(root.editorTop()+height))
	require.Equal(t, regionMessageBar, root.hitTestRegion(root.editorTop()+height+1))
	require.Equal(t, regionOutside, root.hitTestRegion(root.editorTop()+height+2))
	require.Equal(t, strings.Repeat("wrapped draft ", 9), root.editor.Value())
	for _, width := range []int{120, 83, 40} {
		_, _ = root.Update(tea.WindowSizeMsg{Width: width, Height: 30})
		editorWidth, _ := root.editor.GetSize()
		require.Equal(t, width, editorWidth, "editor reports its full outer width")
		for _, component := range []struct{ name, view string }{
			{"editor", root.editor.View()},
			{"chat", root.chatPage.View()},
			{"resize handle", root.renderResizeHandle(width)},
			{"root", root.View().Content},
		} {
			for line := range strings.SplitSeq(component.view, "\n") {
				require.Equal(t, width, ansi.StringWidth(line), "%s must not add editor margins twice", component.name)
			}
		}
	}
}

func TestExternalAttachmentBannerClickUsesExpandedCoordinates(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	defer root.dialogMgr.Cleanup()
	file := filepath.Join(t.TempDir(), "preview.txt")
	require.NoError(t, os.WriteFile(file, []byte("ATTACHMENT-PREVIEW-CONTENT"), 0o600))
	require.NoError(t, root.editor.AttachFile(file))
	draft := root.editor.Value()
	root.resizeAll()
	_ = root.View()
	collapsed := root.editor.BannerHeight()
	require.Equal(t, 3, collapsed)
	// The summary's far-right chevron expands; it is not an attachment target.
	_, _ = root.Update(tea.MouseClickMsg{X: root.width - 3, Y: root.editorTop() - collapsed + 2, Button: tea.MouseLeft})
	require.Equal(t, 4, root.editor.BannerHeight())
	require.False(t, root.dialogMgr.Open())
	_ = root.View()
	bannerTop := root.editorTop() - root.editor.BannerHeight()
	require.Equal(t, regionContextBar, root.hitTestRegion(bannerTop+3))
	_, _ = root.Update(tea.MouseClickMsg{X: 2, Y: bannerTop + 3, Button: tea.MouseLeft})
	require.True(t, root.dialogMgr.Open())
	require.Contains(t, root.dialogMgr.TopDialog().View(), "ATTACHMENT-PREVIEW-CONTENT")
	require.Equal(t, draft, root.editor.Value())
}

func TestActualProgramTabbarDragDropSettlesAndRejectsHiddenHold(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	for _, entry := range []struct{ id, title string }{{"second-full-session-id", "Bravo"}, {"third-full-session-id", "Charlie"}} {
		sess := session.New(session.WithID(entry.id))
		sess.Title = entry.title
		a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
		_, err := root.supervisor.AddSession(t.Context(), a, sess, "", nil)
		require.NoError(t, err)
	}
	writer := &cacheProgramWriter{}
	model := &shellProgramModel{root: root}
	coalescer := tuiinput.NewMouseCoalescer()
	t.Cleanup(coalescer.Stop)
	program := startTestProgram(t, root, model, tea.WithOutput(writer), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return coalescer.Filter(msg) }))
	coalescer.SetSender(program.Send)
	root.supervisor.SetProgram(program)
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("tabbar program snapshot timed out")
			return shellSnapshot{}
		}
	}
	tabs, active := root.supervisor.GetTabs()
	program.Send(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	require.Eventually(t, func() bool { s := snapshot(); return strings.Contains(s.tabView, "Bravo") && s.tabHeight > 0 }, time.Second, time.Millisecond)
	tabPoint := func(s shellSnapshot, label string) (int, int) {
		for y, line := range strings.Split(s.tabView, "\n") {
			before, _, found := strings.Cut(ansi.Strip(line), label)
			if found {
				return ansi.StringWidth(before) + tabFrameOrigin() + 1, s.tabY + y
			}
		}
		t.Fatalf("tab label %q missing from %q", label, s.tabView)
		return 0, 0
	}
	before := snapshot()
	x, y := tabPoint(before, "Bravo")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	pressed := snapshot()
	require.False(t, pressed.overlay, "press alone does not create an overlay")
	require.Equal(t, before.activeID, pressed.activeID, "press never activates source")
	program.Send(tea.MouseMotionMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return snapshot().overlay }, time.Second, time.Millisecond, "coalesced three-cell motion activates the drag")
	grabbed := snapshot()
	targetX, _ := tabPoint(grabbed, "Charlie")
	program.Send(tea.MouseMotionMsg{X: targetX + 12, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return snapshot().overlayX != grabbed.overlayX }, time.Second, time.Millisecond)
	moved := snapshot()
	require.True(t, moved.overlay)
	require.NotEqual(t, grabbed.overlayX, moved.overlayX)
	require.NotEqual(t, grabbed.content, moved.content, "floating overlay motion must invalidate root composition")
	program.Send(tea.MouseMotionMsg{X: 115, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseReleaseMsg{X: 115, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		s := snapshot()
		return len(s.tabIDs) == 3 && s.tabIDs[2] == "second-full-session-id" && !s.overlay && s.active == 0
	}, 2*time.Second, time.Millisecond)
	settled := snapshot()
	require.GreaterOrEqual(t, settled.overlayFrames, 2, "actual program must publish distinct floating overlay positions")
	require.Zero(t, settled.holdMessages, "distance activation schedules no delayed hold messages")
	assertAnimationCadence(t, "tab drag/drop", settled.tickTimes)
	require.GreaterOrEqual(t, len(settled.overlayTimes), 8, "drop must publish multiple cell positions at animation cadence")
	assertAnimationCadence(t, "visible overlay", settled.overlayTimes)
	x, y = tabPoint(settled, "Bravo")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return snapshot().activeID == "second-full-session-id" }, time.Second, time.Millisecond)
	// Resize then click using newly rendered coordinates, not old tab bounds.
	program.Send(tea.WindowSizeMsg{Width: 100, Height: 35})
	resized := snapshot()
	x, y = tabPoint(resized, "profile")
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return snapshot().activeID == "profile" }, time.Second, time.Millisecond)
	// Remove all but one tab while a press is pending. Later coalesced motion
	// and release cannot resurrect the vanished source or emit a delayed hold.
	resized = snapshot()
	x, y = tabPoint(resized, "Bravo")
	holds := resized.holdMessages
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	program.Send(messages.TabsUpdatedMsg{Tabs: []messages.TabInfo{{SessionID: "profile", Title: "profile", IsActive: true}}, ActiveIdx: 0})
	program.Send(tea.MouseMotionMsg{X: x + 8, Y: y, Button: tea.MouseLeft})
	program.Send(tea.MouseReleaseMsg{X: x + 8, Y: y, Button: tea.MouseLeft})
	require.Equal(t, holds, snapshot().holdMessages, "no timer is armed by pending press")
	hidden := snapshot()
	require.Positive(t, hidden.tabHeight, "one session remains visible")
	require.False(t, hidden.overlay)
	require.Zero(t, hidden.active)
	require.Eventually(t, func() bool { return snapshot().delayMessages > 0 }, time.Second, time.Millisecond, "SetTabs scroll-delay commands must reach root")
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled awaiting terminal flush")
	}
	writes := len(writer.snapshot())
	require.Never(t, func() bool { return len(writer.snapshot()) != writes }, 80*time.Millisecond, time.Millisecond, "hidden settled tabbar must stop terminal writes")
}

func assertAnimationCadence(t *testing.T, label string, times []time.Time) {
	t.Helper()
	require.GreaterOrEqual(t, len(times), 8, label)
	elapsed := times[len(times)-1].Sub(times[0])
	hz := float64(len(times)-1) / elapsed.Seconds()
	t.Logf("%s: %d samples over %s = %.1f Hz", label, len(times), elapsed, hz)
	require.Greater(t, hz, 25.0, "%s must exceed the former 14Hz ceiling", label)
}

func TestActualProgramEditorDirectResizeAndPostSendCollapse(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	writer := &cacheProgramWriter{}
	coalescer := tuiinput.NewMouseCoalescer()
	t.Cleanup(coalescer.Stop)
	model := &shellProgramModel{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(writer), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return coalescer.Filter(msg) }))
	coalescer.SetSender(program.Send)
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("editor snapshot timed out")
			return shellSnapshot{}
		}
	}
	initial := snapshot()
	require.Equal(t, 1, initial.editorHeight)
	program.Send(tea.MouseClickMsg{X: 40, Y: initial.tabY - 1, Button: tea.MouseLeft})
	require.True(t, snapshot().resizeDragging)
	program.Send(tea.MouseMotionMsg{X: 40, Y: initial.tabY - 10, Button: tea.MouseLeft})
	require.Eventually(t, func() bool {
		s := snapshot()
		return s.resizeDragging && !s.heightMoving && s.editorHeight > initial.editorHeight && s.editorHeight == s.editorTarget
	}, time.Second, time.Millisecond, "each accepted coalesced motion allocates directly")
	first := snapshot()
	require.Equal(t, initial.ticks, first.ticks, "manual motion needs no animation frame lease")
	program.Send(tea.MouseMotionMsg{X: 40, Y: initial.tabY - 11, Button: tea.MouseLeft})
	// Release flushes the pending coalesced motion before ending the gesture.
	program.Send(tea.MouseReleaseMsg{X: 40, Y: initial.tabY - 11, Button: tea.MouseLeft})
	expanded := snapshot()
	require.False(t, expanded.resizeDragging)
	require.False(t, expanded.heightMoving)
	require.Equal(t, first.editorHeight+1, expanded.editorHeight, "release applies the final accepted pointer without chasing")
	require.Equal(t, expanded.editorTarget, expanded.editorHeight, "empty input honors manual resize")
	require.Zero(t, expanded.active)
	require.Equal(t, initial.ticks, expanded.ticks)
	// Automatic content growth remains immediate. Sending clears the manual
	// request and animates back to the automatic one-line empty-input target.
	for i, line := range []string{"first", "second", "third", "fourth", "fifth", "sixth"} {
		if i > 0 {
			program.Send(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		}
		program.Send(tea.PasteMsg{Content: line})
	}
	require.Contains(t, snapshot().editor, "sixth")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	sent := snapshot()
	require.Empty(t, sent.editor)
	require.Equal(t, 1, sent.editorTarget)
	require.Greater(t, sent.editorHeight, 1, "send must not snap the displayed editor to one line")
	require.True(t, sent.heightMoving)
	neverInTestLoop(t, func() bool { return snapshot().editorHeight != sent.editorHeight }, 250*time.Millisecond, "post-send collapse holds before its animation")
	require.Eventually(t, func() bool { s := snapshot(); return s.editorHeight < sent.editorHeight && s.editorHeight > 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { s := snapshot(); return s.editorHeight == 1 && !s.heightMoving }, 2*time.Second, time.Millisecond)
	// The fake runtime leaves a pending-response spinner. Its 100ms frames
	// must not cause whole-root composition on every shared 60Hz tick.
	assertAnimationCadence(t, "automatic editor collapse", snapshot().tickTimes)
	busy := snapshot()
	select {
	case <-time.After(400 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled measuring spinner cadence")
	}
	afterBusy := snapshot()
	ticks, compositions := afterBusy.ticks-busy.ticks, afterBusy.compositions-busy.compositions
	t.Logf("pending spinner over400ms: %d accepted ticks, %d root compositions", ticks, compositions)
	require.GreaterOrEqual(t, ticks, 10)
	require.Less(t, compositions, ticks/2)
	program.Send(runtime.StreamStopped("profile", "root", "completed"))
	require.Eventually(t, func() bool { return snapshot().active == 0 }, time.Second, time.Millisecond)
	settled := snapshot()
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled awaiting terminal flush")
	}
	writes := len(writer.snapshot())
	neverInTestLoop(t, func() bool { return len(writer.snapshot()) != writes || snapshot().ticks != settled.ticks }, 80*time.Millisecond, "settled editor must not write or tick")
	program.Send(tea.PasteMsg{Content: "first-idle-input"})
	require.Equal(t, "first-idle-input", snapshot().editor, "first idle input is applied in its event, without a frame lease")
	program.Send(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	program.Send(tea.PasteMsg{Content: "automatic-second-line"})
	require.Equal(t, 2, snapshot().editorHeight, "automatic multiline growth remains immediate after manual request clears")
	program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	automatic := snapshot()
	require.True(t, automatic.heightMoving)
	require.Equal(t, 2, automatic.editorHeight, "automatic multiline send also animates rather than snapping")
	require.Equal(t, 1, automatic.editorTarget)
	for i, line := range []string{"new", "draft", "during-collapse"} {
		if i > 0 {
			program.Send(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		}
		program.Send(tea.PasteMsg{Content: line})
	}
	resumed := snapshot()
	require.Equal(t, 3, resumed.editorHeight, "new content grows immediately even during a collapse")
	require.False(t, resumed.heightMoving, "growth cancels the stale collapse target")
}
