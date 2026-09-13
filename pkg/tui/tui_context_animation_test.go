package tui

import (
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
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestEditorShrinkDelayUsesSharedClockAndCancelsOnManualResize(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	root.editor.SetValue("one\ntwo\nthree")
	root.resizeAll()
	require.Equal(t, 3, root.editorHeight)
	_, _ = root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, root.editorHeightMotion.Running())
	require.Equal(t, editorShrinkDelay+animation.ShortDuration, root.editorHeightMotion.Duration())
	delayFraction := float64(editorShrinkDelay) / float64(editorShrinkDelay+animation.ShortDuration)
	require.Zero(t, delayedEditorShrink(delayFraction))
	require.Zero(t, delayedEditorShrink(delayFraction/2))
	require.Greater(t, delayedEditorShrink(delayFraction+0.1), 0.0)
	require.InDelta(t, 1, delayedEditorShrink(1), 0.00001)
	root.handleEditorResize(root.contentHeight - 5)
	require.False(t, root.editorShrinkDelayed)
	require.Equal(t, animation.ShortDuration, root.editorHeightMotion.Duration(), "manual drag replaces the pending hold immediately")
	root.editor.SetValue(strings.Repeat("growth\n", 12))
	root.manualEditorHeight = 0
	root.resizeAll()
	require.False(t, root.editorHeightMotion.Running(), "genuine automatic growth cancels stale collapse")
	require.Equal(t, root.editorHeightTarget, root.editorHeight)
}

func TestActualProgramContextUsageLifecycleAndIdleCleanup(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	second := session.New(session.WithID("context-second"))
	secondApp := app.New(t.Context(), nil, second, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	_, err := root.supervisor.AddSession(t.Context(), secondApp, second, "", nil)
	require.NoError(t, err)
	// Build the background page without changing the active session pointers.
	page, state, ed, application := root.chatPage, root.sessionState, root.editor, root.application
	root.initSessionComponents("context-second", secondApp, second)
	root.chatPage, root.sessionState, root.editor, root.application = page, state, ed, application
	writer := &cacheProgramWriter{}
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(writer))
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("context snapshot timed out")
			return shellSnapshot{}
		}
	}
	initial := snapshot()
	require.Contains(t, initial.contextView, "Context 0%")
	require.Zero(t, initial.active)
	usage := runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 75, ContextLimit: 100})
	program.Send(messages.SessionRuntimeEventMsg{Event: usage})
	require.Eventually(t, func() bool {
		s := snapshot()
		return s.active > 0 && !strings.Contains(s.contextView, "Context 0%") && !strings.Contains(s.contextView, "Context 75%")
	}, time.Second, time.Millisecond, "accepted shared frames animate the actual token strip")
	program.Send(messages.SessionRuntimeEventMsg{Event: usage})
	program.Send(tea.WindowSizeMsg{Width: 83, Height: 29})
	require.Equal(t, 79, ansi.StringWidth(snapshot().contextView))
	require.Eventually(t, func() bool { s := snapshot(); return s.active == 0 && strings.Contains(s.contextView, "Context 75%") }, time.Second, time.Millisecond)
	// A same-agent background session must not overwrite this session's usage.
	program.Send(messages.RoutedMsg{SessionID: "context-second", Inner: messages.SessionRuntimeEventMsg{Event: runtime.NewTokenUsageEvent("context-second", "root", &runtime.Usage{ContextLength: 20, ContextLimit: 100})}})
	require.Contains(t, snapshot().contextView, "Context 75%")
	program.Send(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 90, ContextLimit: 100}))
	require.Positive(t, snapshot().active)
	program.Send(messages.SwitchTabMsg{SessionID: "context-second"})
	require.NotContains(t, snapshot().contextView, "Context 20%", "known destination animates from displayed position")
	program.Send(runtime.AgentInfo("root", "test/model", "", "", 100))
	require.Eventually(t, func() bool {
		s := snapshot()
		return strings.Contains(s.content, "Context ") && !strings.Contains(s.contextView, "Context 20%") && !strings.Contains(s.contextView, "Context 75%") && !strings.Contains(s.contextView, "Context 0%")
	}, time.Second, time.Millisecond, "tab change renders a nonzero intermediate usage")
	require.Eventually(t, func() bool { return strings.Contains(snapshot().contextView, "Context 20%") }, time.Second, time.Millisecond)
	program.Send(messages.SwitchTabMsg{SessionID: "profile"})
	require.NotContains(t, snapshot().contextView, "Context 90%", "return animates to the selected session's canonical target")
	program.Send(messages.SwitchTabMsg{SessionID: "context-second"})
	program.Send(messages.SwitchTabMsg{SessionID: "profile"})
	program.Send(messages.RoutedMsg{SessionID: "context-second", Inner: messages.SessionRuntimeEventMsg{Event: runtime.NewTokenUsageEvent("context-second", "root", &runtime.Usage{ContextLength: 1, ContextLimit: 100})}})
	require.Eventually(t, func() bool { return strings.Contains(snapshot().contextView, "Context 90%") }, time.Second, time.Millisecond, "late background updates cannot pull the selected target back")
	program.Send(messages.SessionRuntimeEventMsg{Event: &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: &session.Session{ID: "profile"}}}})
	require.Contains(t, snapshot().contextView, "Context 0%", "projection reset drops stale usage")
	require.Eventually(t, func() bool { return snapshot().active == 0 }, time.Second, time.Millisecond)
	settled := snapshot()
	require.Never(t, func() bool { return snapshot().ticks != settled.ticks }, 80*time.Millisecond, time.Millisecond, "settled context has no tick lease")
	program.Send(tea.PasteMsg{Content: "immediate idle input"})
	require.Equal(t, "immediate idle input", snapshot().editor)
}

func TestContextUsageCleanupAndLeanModeReleaseLeases(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	_, _ = root.Update(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 50, ContextLimit: 100}))
	require.Positive(t, root.ar.ActiveCount())
	root.leanMode = true
	root.syncContextBar()
	require.Zero(t, root.ar.ActiveCount())
	root.leanMode = false
	root.syncContextBar()
	require.Positive(t, root.ar.ActiveCount())
	root.cleanupAll()
	require.Nil(t, root.syncContextBar(), "late events cannot rearm a closed root")
	require.Zero(t, root.ar.ActiveCount(), "teardown cancels context and editor transitions")
}

func TestContextUsageHydratesCanonicalRestoredSnapshotAndInertGeometry(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	root.application.Session().SetUsage(35, 5)
	_, _ = root.Update(runtime.AgentInfo("root", "test/model", "", "", 100))
	require.EqualValues(t, 40, root.contextUsage["profile"]["root"].ContextLength)
	require.EqualValues(t, 100, root.contextUsage["profile"]["root"].ContextLimit)
	restored := &session.Session{ID: "profile"}
	restored.SetUsage(15, 5)
	_, _ = root.Update(messages.SessionRuntimeEventMsg{Event: &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: restored}}})
	require.Contains(t, root.contextBar.View(), "Context 20%")
	require.Zero(t, root.ar.ActiveCount(), "restored projection directly seeds its canonical usage")
	_, editorHeight := root.editor.GetSize()
	stripY := root.editorTop() + editorHeight
	require.Equal(t, regionContextUsage, root.hitTestRegion(stripY))
	before := root.editor.Value()
	_, cmd := root.Update(tea.MouseClickMsg{X: 40, Y: stripY, Button: tea.MouseLeft})
	require.Nil(t, cmd, "usage strip has no invented click action")
	require.False(t, root.dialogMgr.Open())
	require.False(t, root.isDragging)
	require.Equal(t, before, root.editor.Value())
}

func TestActualProgramContextSurvivesStreamingPartialUsage(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.storeContextUsage("profile", "root", runtime.Usage{ContextLength: 20, ContextLimit: 100})
	root.contextBar.SetContextUsageDirect(20, 100)
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("stream snapshot timed out")
			return shellSnapshot{}
		}
	}
	send := func(event runtime.Event) { program.Send(messages.SessionRuntimeEventMsg{Event: event}) }
	visible := func(s shellSnapshot) {
		require.Contains(t, ansi.Strip(s.content), "Context ")
		require.Equal(t, 116, ansi.StringWidth(s.contextView))
		require.Contains(t, s.contextView, "▄")
	}
	send(runtime.StreamStarted("profile", "root"))
	visible(snapshot())
	require.Contains(t, snapshot().contextView, "Context 20%")
	send(runtime.AgentChoice("root", "profile", "streaming before usage"))
	send(runtime.AgentInfo("root", "test/model", "", "", 0))
	send(runtime.NewTokenUsageEvent("profile", "root", nil))
	send(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{OutputTokens: 3}))
	require.Contains(t, snapshot().contextView, "Context 20%")
	send(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 65}))
	require.Eventually(t, func() bool {
		s := snapshot()
		visible(s)
		return !strings.Contains(s.contextView, "Context 20%") && !strings.Contains(s.contextView, "Context 65%")
	}, time.Second, time.Millisecond)
	intermediate := snapshot()
	send(runtime.NewTokenUsageEvent("profile", "helper", &runtime.Usage{ContextLength: 99, ContextLimit: 100}))
	require.NotContains(t, snapshot().contextView, "Context 99%")
	send(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 85, ContextLimit: 100}))
	require.NotContains(t, snapshot().contextView, "Context 85%", "retarget must not snap")
	send(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{Cost: 0.1}))
	require.Eventually(t, func() bool { s := snapshot(); visible(s); return strings.Contains(s.contextView, "Context 85%") }, time.Second, time.Millisecond)
	require.Greater(t, snapshot().ticks, intermediate.ticks)
	send(runtime.StreamStopped("profile", "root", "completed"))
	require.Eventually(t, func() bool { return snapshot().active == 0 }, time.Second, time.Millisecond)
	settled := snapshot()
	visible(settled)
	require.Contains(t, settled.contextView, "Context 85%")
	require.Never(t, func() bool { return snapshot().ticks != settled.ticks }, 80*time.Millisecond, time.Millisecond)
}

func TestContextUsagePartialMetadataAndExplicitZero(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	defer root.ar.Stop()
	root.storeContextUsage("profile", "root", runtime.Usage{ContextLength: 60, ContextLimit: 100})
	root.trackContextUsage(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{OutputTokens: 5}), root.application.Session())
	require.EqualValues(t, 60, root.contextUsage["profile"]["root"].ContextLength)
	require.EqualValues(t, 100, root.contextUsage["profile"]["root"].ContextLimit)
	root.trackContextUsage(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 0, ContextLimit: 100}), root.application.Session())
	require.Zero(t, root.contextUsage["profile"]["root"].ContextLength, "a complete empty-window snapshot is authoritative")
	root.trackContextUsage(runtime.NewTokenUsageEvent("profile", "root", &runtime.Usage{ContextLength: 25}), root.application.Session())
	require.EqualValues(t, 25, root.contextUsage["profile"]["root"].ContextLength)
	require.EqualValues(t, 100, root.contextUsage["profile"]["root"].ContextLimit)
	root.trackContextUsage(runtime.AgentInfo("root", "test/model", "", "", 0), root.application.Session())
	require.EqualValues(t, 100, root.contextUsage["profile"]["root"].ContextLimit, "unknown metadata does not clear a known window")
}
