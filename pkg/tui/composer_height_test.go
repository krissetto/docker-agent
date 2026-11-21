package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const composerDraft = "first\nsecond\nthird\nfourth\nfifth\nsixth\nlast"

func TestComposerGrowsForTypedRows(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	_, initial := root.activeTab.editor.GetSize()
	initialContent := root.contentHeight
	for _, r := range composerDraft {
		if r == '\n' {
			root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		} else {
			root.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	require.Equal(t, composerDraft, root.activeTab.editor.Value())
	_, grown := root.activeTab.editor.GetSize()
	require.Equal(t, initial+6, grown)
	require.Equal(t, initialContent-6, root.contentHeight)
	view := ansi.Strip(root.activeTab.editor.View())
	require.Contains(t, view, "first")
	require.Contains(t, view, "last")
}

func TestComposerGrowsWhenTerminalWidthRewrapsDraft(t *testing.T) {
	const wrapped = "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen SIXTEEN"
	root, _, _ := frozenClockRoot(t, 80, 30)
	root.Update(messages.RestorePendingMessagesMsg{Content: wrapped})
	_, wide := root.activeTab.editor.GetSize()
	root.Update(tea.WindowSizeMsg{Width: 18, Height: 30})
	_, reflowed := root.activeTab.editor.GetSize()
	require.Greater(t, reflowed, wide)
	view := ansi.Strip(root.activeTab.editor.View())
	require.Contains(t, view, "one two")
	require.Contains(t, view, "SIXTEEN")
}

// Capture the owner's timer commands without executing unrelated editor or chat IO.
type composerScheduler struct {
	rootImmediateScheduler

	pending []tea.Cmd
}

func (s *composerScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	cmd := s.rootImmediateScheduler.Tick(delay, create)
	s.pending = append(s.pending, cmd)
	return cmd
}

func TestComposerShrinkHoldsThenEasesActualViewport(t *testing.T) {
	scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root, _, _ := harnessRoot(t, 80, 30, animation.NewRuntimeWithScheduler(scheduler))
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	_, grown := root.activeTab.editor.GetSize()
	before := root.View().Content
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Empty(t, root.activeTab.editor.Value())
	shrinking := root.View().Content
	require.NotEqual(t, before, shrinking)
	started := root.ar.Now()
	lastHeight, intermediate := grown, false
	for range 100 {
		if !root.composer.motion.Running() {
			break
		}
		require.NotEmpty(t, scheduler.pending)
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		root.Update(cmd())
		_, height := root.activeTab.editor.GetSize()
		if root.ar.Now()-started <= editorShrinkDelay {
			require.Equal(t, grown, height)
			require.Equal(t, shrinking, root.View().Content)
		}
		require.LessOrEqual(t, height, lastHeight)
		intermediate = intermediate || height < grown && height > initialComposerHeight()
		lastHeight = height
	}
	require.True(t, intermediate)
	require.Equal(t, initialComposerHeight(), lastHeight)
	require.False(t, root.composer.motion.Running())
	require.NotEqual(t, shrinking, root.View().Content)
}

func TestComposerRegrowthCancelsPendingShrink(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	require.True(t, root.composer.motion.Running())
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft + "\nmore"})
	require.False(t, root.composer.motion.Running())
	_, height := root.activeTab.editor.GetSize()
	require.Equal(t, initialComposerHeight()+7, height)
}

func TestComposerGripPinsHeightAcrossEditsAndTerminalClamps(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	require.True(t, root.composer.motion.Running())

	// Move up two rows using the upstream pointer path and release. The
	// chosen allocation should immediately replace the in-flight shrink.
	y := root.contentHeight
	root.Update(tea.MouseClickMsg{X: 40, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: 40, Y: y - 2, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: 40, Y: y - 2, Button: tea.MouseLeft})
	require.False(t, root.composer.motion.Running())
	_, pinned := root.activeTab.editor.GetSize()
	require.Equal(t, initialComposerHeight()+8, pinned)
	require.Equal(t, y-2, root.contentHeight)

	root.Update(messages.RestorePendingMessagesMsg{Content: strings.Repeat("long draft\n", 15)})
	_, height := root.activeTab.editor.GetSize()
	require.Equal(t, pinned, height)
	root.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	_, height = root.activeTab.editor.GetSize()
	require.Equal(t, 1+styles.EditorStyle.GetVerticalFrameSize(), height)
	require.Equal(t, 1, root.contentHeight)
	require.Equal(t, 6, lipgloss.Height(root.View().Content))
	root.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	_, height = root.activeTab.editor.GetSize()
	require.Equal(t, pinned, height)
}

func TestComposerBudgetsSeparateAttachmentRows(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 200, 30)
	name := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(name, []byte("attachment"), 0o600))
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft + "\n"})
	_, before := root.activeTab.editor.GetSize()
	root.Update(messages.InsertFileRefMsg{FilePath: name})
	require.Equal(t, 3, root.activeTab.editor.BannerHeight())
	_, after := root.activeTab.editor.GetSize()
	require.GreaterOrEqual(t, after, before, "attachment rows do not consume the editable viewport")
	view := ansi.Strip(root.activeTab.editor.View())
	require.Contains(t, ansi.Strip(root.composerView()), filepath.Base(name))
	require.Contains(t, view, "first", "banner must not take an editable row")
	require.Contains(t, view, "last")
	require.Equal(t, regionEditor, root.hitTestRegion(root.editorTop()+after-1))

	root.Update(tea.WindowSizeMsg{Width: 18, Height: 30})
	width, _ := root.activeTab.editor.GetSize()
	require.Equal(t, 18, width)
	require.LessOrEqual(t, lipgloss.Width(root.activeTab.editor.View()), width)
}

func initialComposerHeight() int {
	return composerMinRows + styles.EditorStyle.GetVerticalFrameSize()
}

func (s *composerScheduler) step(t *testing.T, root *appModel) {
	t.Helper()
	require.NotEmpty(t, s.pending)
	cmd := s.pending[0]
	s.pending = s.pending[1:]
	root.Update(cmd())
}

func assertComposerMotion(t *testing.T, root *appModel, scheduler *composerScheduler, from, to int, hold time.Duration) {
	t.Helper()
	require.Equal(t, from, root.composer.rows)
	require.Equal(t, to, root.composer.target)
	require.True(t, root.composer.motion.Running())
	require.Equal(t, hold+350*time.Millisecond, root.composer.motion.Duration())
	start := root.ar.Now()
	prior := from
	intermediate := false
	for range 100 {
		if !root.composer.motion.Running() {
			break
		}
		scheduler.step(t, root)
		rows := root.composer.rows
		if root.ar.Now()-start <= hold {
			require.Equal(t, from, rows)
		}
		if from < to {
			require.GreaterOrEqual(t, rows, prior)
			require.LessOrEqual(t, rows, to)
		} else {
			require.LessOrEqual(t, rows, prior)
			require.GreaterOrEqual(t, rows, to)
		}
		intermediate = intermediate || rows != from && rows != to
		_, allocated := root.activeTab.editor.GetSize()
		require.Equal(t, rows+styles.EditorStyle.GetVerticalFrameSize(), allocated)
		prior = rows
	}
	require.True(t, intermediate, "the transition paints intermediate allocations")
	require.False(t, root.composer.motion.Running())
	require.Equal(t, to, root.composer.rows)
	require.LessOrEqual(t, root.ar.Now()-start, hold+350*time.Millisecond+animation.TickRate)
}

func TestComposerHistoryRecallAnimatesAndTypingInterrupts(t *testing.T) {
	for _, direction := range []struct {
		name string
		key  rune
		from int
	}{{"Up", tea.KeyUp, 2}, {"Down", tea.KeyDown, 0}} {
		t.Run(direction.name, func(t *testing.T) {
			scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
			root, _, _ := harnessRoot(t, 80, 40, animation.NewRuntimeWithScheduler(scheduler))
			require.NoError(t, root.history.Load())
			root.history.Messages = []string{"older", composerDraft, "newer"}
			root.history.SetCurrent(direction.from)
			require.Equal(t, 1, root.composer.rows)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			require.Equal(t, composerDraft, root.activeTab.editor.Value())
			assertComposerMotion(t, root, scheduler, 1, 7, 0)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			assertComposerMotion(t, root, scheduler, 7, 1, 500*time.Millisecond)

			root.history.SetCurrent(direction.from)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			scheduler.step(t, root)
			require.Less(t, root.composer.rows, 7)
			root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			require.Equal(t, composerDraft+"x", root.activeTab.editor.Value())
			require.Equal(t, 7, root.composer.rows, "typing restores the full allocation even with unchanged row count")
			require.False(t, root.composer.motion.Running())
			root.Update(tea.KeyPressMsg{Code: direction.key})
			require.Equal(t, composerDraft+"x", root.activeTab.editor.Value(), "arrows move the cursor in a typed draft")
		})
	}
}

func TestComposerFocusCollapsePreservesDraftAndManualAllocation(t *testing.T) {
	scheduler := &composerScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root, _, _ := harnessRoot(t, 80, 40, animation.NewRuntimeWithScheduler(scheduler))
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.handleEditorResize(root.contentHeight - 2)
	initialContent := root.contentHeight
	require.Equal(t, 9, root.composer.rows)

	root.Update(messages.RequestFocusMsg{Target: messages.PanelMessages})
	require.Equal(t, PanelContent, root.focusedPanel)
	assertComposerMotion(t, root, scheduler, 9, 1, 0)
	require.Equal(t, initialContent+8, root.contentHeight)
	require.Equal(t, composerDraft, root.activeTab.editor.Value())

	root.Update(tea.MouseClickMsg{X: 8, Y: root.editorTop() + 1, Button: tea.MouseLeft})
	require.Equal(t, PanelEditor, root.focusedPanel)
	assertComposerMotion(t, root, scheduler, 1, 9, 0)
	require.Equal(t, composerDraft, root.activeTab.editor.Value())
	require.Equal(t, initialContent, root.contentHeight)
}
