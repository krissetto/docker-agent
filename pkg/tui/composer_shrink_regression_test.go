package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// Capture only the owner's scheduler commands. Delivering these runs the real
// Runtime.Continue/Accept path without executing send, provider or editor IO
// commands returned alongside it by Update.
type composerShrinkScheduler struct {
	rootImmediateScheduler

	pending []tea.Cmd
}

func (s *composerShrinkScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	cmd := s.rootImmediateScheduler.Tick(delay, create)
	s.pending = append(s.pending, cmd)
	return cmd
}

func composerShrinkRoot(t *testing.T, lean bool) (*appModel, *composerShrinkScheduler) {
	t.Helper()
	root := splitTestRoot(t)
	root.ar.Stop()
	scheduler := &composerShrinkScheduler{rootImmediateScheduler: rootImmediateScheduler{now: time.Unix(1, 0)}}
	root.ar = animation.NewRuntimeWithScheduler(scheduler)
	root.leanMode = lean
	root.focusedPanel = PanelEditor
	root.editor.Focus()
	root.resizeAll()
	return root, scheduler
}

func (s *composerShrinkScheduler) step(t *testing.T, root *appModel) {
	t.Helper()
	before := root.ar.Now()
	for range 100 {
		require.NotEmpty(t, s.pending, "automatic shrink must retain the shared owner's continuation")
		cmd := s.pending[0]
		s.pending = s.pending[1:]
		root.Update(cmd())
		root.View()
		if root.ar.Now() > before {
			return
		}
	}
	t.Fatal("scheduler commands did not deliver an accepted owner tick")
}

func assertComposerDelayedShrink(t *testing.T, root *appModel, scheduler *composerShrinkScheduler, from, target int) {
	t.Helper()
	require.Greater(t, from, target)
	require.Equal(t, from, root.editorHeight, "trigger must not shrink the displayed allocation")
	require.Equal(t, target, root.editorHeightTarget)
	require.True(t, root.editorShrinkDelayed)
	require.True(t, root.editorHeightMotion.Running())
	require.Equal(t, editorShrinkDelay+editorShrinkDuration, root.editorHeightMotion.Duration())
	started := root.ar.Now()
	previous := from
	sawEasing, sawIntermediate := false, false
	for range 100 {
		if !root.editorHeightMotion.Running() {
			break
		}
		scheduler.step(t, root)
		elapsed := root.ar.Now() - started
		if elapsed <= editorShrinkDelay {
			require.Equal(t, from, root.editorHeight, "every initial shared-clock frame must hold the original height")
			require.Zero(t, root.editorHeightMotion.Value())
		} else if root.editorHeightMotion.Running() {
			value := root.editorHeightMotion.Value()
			sawEasing = sawEasing || value > 0 && value < 1
		}
		require.LessOrEqual(t, root.editorHeight, previous)
		require.GreaterOrEqual(t, root.editorHeight, target)
		sawIntermediate = sawIntermediate || root.editorHeight < from && root.editorHeight > target
		previous = root.editorHeight
		// Repeated layout must not restart the hold or acquire another lease.
		root.resizeAll()
	}
	require.True(t, sawEasing, "hold must be followed by the existing fluid easing")
	if from-target > 1 {
		require.True(t, sawIntermediate, "multi-row collapse must paint intermediate allocations")
	}
	require.Equal(t, target, root.editorHeight)
	require.False(t, root.editorHeightMotion.Running())
	require.False(t, root.editorShrinkDelayed)
	require.LessOrEqual(t, root.ar.Now()-started, editorShrinkDelay+editorShrinkDuration+animation.TickRate)
}

func TestComposerAutomaticShrinkTriggersAlwaysDelay(t *testing.T) {
	const draft = "one\ntwo\nthree\nfour\nfive\n"
	for _, lean := range []bool{false, true} {
		name := "full"
		if lean {
			name = "lean"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				prepare func(*appModel)
				trigger tea.Msg
				target  int
			}{
				{name: "backspace-nonempty", trigger: tea.KeyPressMsg{Code: tea.KeyBackspace}, target: 5},
				{name: "send-clear", trigger: tea.KeyPressMsg{Code: tea.KeyEnter}, target: 1},
				{name: "follow-up-send-clear", trigger: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, target: 1},
				{name: "restore-shorter-draft", trigger: messages.RestorePendingMessagesMsg{Content: "short"}, target: 1},
				{name: "clear-draft", trigger: messages.RestorePendingMessagesMsg{}, target: 1},
				{name: "history-next", prepare: func(root *appModel) {
					root.history.Messages = []string{draft, "short"}
					root.history.SetCurrent(1)
					root.editor.SetValue("")
					root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
				}, trigger: tea.KeyPressMsg{Code: tea.KeyDown}, target: 1},
				{name: "history-search-clear", trigger: tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}, target: 1},
				{name: "completion-replacement", prepare: func(root *appModel) {
					root.handleWindowResize(30, 40)
					root.editor.SetValue("")
					root.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
					root.Update(tea.PasteMsg{Content: strings.Repeat("a", 140)})
					// A key update refreshes the active completion's trigger word.
					root.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
				}, trigger: completion.SelectedMsg{Value: "short"}, target: 1},
				{name: "cached-tab-switch", trigger: messages.SwitchTabMsg{SessionID: "second"}, target: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root, scheduler := composerShrinkRoot(t, lean)
					root.editor.SetValue(draft)
					if tc.prepare != nil {
						tc.prepare(root)
					}
					root.resizeAll()
					from := root.editorHeight
					require.Greater(t, from, tc.target)
					root.Update(tc.trigger)
					assertComposerDelayedShrink(t, root, scheduler, from, tc.target)
				})
			}
		})
	}
}

func TestComposerAutomaticShrinkRewrapAndTerminalClamp(t *testing.T) {
	for _, lean := range []bool{false, true} {
		root, scheduler := composerShrinkRoot(t, lean)
		root.handleWindowResize(30, 40)
		root.editor.SetValue(strings.Repeat("word ", 30))
		root.resizeAll()
		from := root.editorHeight
		root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		target := root.editor.ContentLineCount()
		assertComposerDelayedShrink(t, root, scheduler, from, target)

		root.editor.SetValue(strings.Repeat("row\n", 10))
		root.resizeAll()
		root.editor.SetValue("short")
		root.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
		// The terminal safety ceiling yields immediately, but content below
		// that ceiling still receives the complete canonical hold and ease.
		assertComposerDelayedShrink(t, root, scheduler, root.composerMaxTextHeight(), 1)
	}
}

func TestComposerAutomaticShrinkRetargetGrowthAndManualInterrupt(t *testing.T) {
	root, scheduler := composerShrinkRoot(t, false)
	root.editor.SetValue(strings.Repeat("row\n", 10))
	root.resizeAll()
	root.Update(messages.RestorePendingMessagesMsg{Content: "one\ntwo\nthree\nfour"})
	// A higher target still below the painted height is another automatic
	// shrink: it must retarget from that height, never snap down to the draft.
	root.Update(messages.RestorePendingMessagesMsg{Content: "one\ntwo\nthree\nfour\nfive\nsix"})
	assertComposerDelayedShrink(t, root, scheduler, 11, 6)
	root.editor.SetValue(strings.Repeat("row\n", 10))
	root.resizeAll()
	root.Update(messages.RestorePendingMessagesMsg{Content: "one\ntwo\nthree\nfour"})
	for range 100 {
		if root.editorHeight < 11 {
			break
		}
		scheduler.step(t, root)
	}
	require.Less(t, root.editorHeight, 11)
	from := root.editorHeight
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	assertComposerDelayedShrink(t, root, scheduler, from, 1)

	root.editor.SetValue("one\ntwo\nthree\nfour\nfive\nsix")
	root.resizeAll()
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	y := root.composerLayout().separatorTop
	root.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: 4, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: 4, Y: y, Button: tea.MouseLeft})
	require.Zero(t, root.manualEditorHeight, "no-motion pickup cannot claim manual mode during default shrink")
	assertComposerDelayedShrink(t, root, scheduler, 6, 1)
	root.editor.SetValue("one\ntwo\nthree\nfour\nfive\nsix")
	root.resizeAll()
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	scheduler.step(t, root)
	root.Update(messages.RestorePendingMessagesMsg{Content: strings.Repeat("row\n", 8)})
	require.Equal(t, 9, root.editorHeight, "growth remains immediate during the hold")
	require.False(t, root.editorHeightMotion.Running())

	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	scheduler.step(t, root)
	root.handleEditorResize(root.composerLayout().separatorTop + 3)
	require.Equal(t, 6, root.editorHeight, "real manual movement allocates directly")
	require.Equal(t, 6, root.manualEditorHeight)
	require.False(t, root.editorHeightMotion.Running())
	root.handleWindowResize(40, 12)
	require.Equal(t, root.composerMaxTextHeight(), root.editorHeight)
	require.Equal(t, 6, root.manualEditorHeight)
	root.handleWindowResize(120, 40)
	require.Equal(t, 6, root.editorHeight, "terminal clamp preserves the manual request")
}
