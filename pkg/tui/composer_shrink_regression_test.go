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
		_, renderedHeight := root.editor.GetSize()
		require.Equal(t, root.editorHeight+root.editorFrame().GetVerticalFrameSize(), renderedHeight, "eased shrink must resize the actual editor viewport")
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
						for range 100 {
							if !root.editorHeightMotion.Running() {
								break
							}
							scheduler.step(t, root)
						}
						require.False(t, root.editorHeightMotion.Running())
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

func assertComposerHistoryGrowth(t *testing.T, root *appModel, scheduler *composerShrinkScheduler, from, target int) {
	t.Helper()
	require.Equal(t, from, root.editorHeight, "history recall must not snap the allocation")
	require.Equal(t, target, root.editorHeightTarget)
	require.True(t, root.editorHeightMotion.Running())
	require.False(t, root.editorShrinkDelayed)
	require.Equal(t, editorShrinkDuration, root.editorHeightMotion.Duration())
	started := root.ar.Now()
	previous := from
	sawIntermediate := false
	for range 100 {
		if !root.editorHeightMotion.Running() {
			break
		}
		scheduler.step(t, root)
		require.GreaterOrEqual(t, root.editorHeight, previous)
		require.LessOrEqual(t, root.editorHeight, target)
		sawIntermediate = sawIntermediate || root.editorHeight > from && root.editorHeight < target
		_, renderedHeight := root.editor.GetSize()
		require.Equal(t, root.editorHeight+root.editorFrame().GetVerticalFrameSize(), renderedHeight, "animated rows must reach the actual editor viewport")
		previous = root.editorHeight
		root.resizeAll()
	}
	require.True(t, sawIntermediate, "history growth must paint intermediate allocations, not just run a timer")
	require.Equal(t, target, root.editorHeight)
	require.False(t, root.editorHeightMotion.Running())
	require.LessOrEqual(t, root.ar.Now()-started, editorShrinkDuration+animation.TickRate)
}

func assertComposerSchedulerIdle(t *testing.T, root *appModel, scheduler *composerShrinkScheduler) {
	t.Helper()
	for range 100 {
		if len(scheduler.pending) == 0 {
			break
		}
		cmd := scheduler.pending[0]
		scheduler.pending = scheduler.pending[1:]
		root.Update(cmd())
		root.View()
	}
	require.Zero(t, root.ar.ActiveCount(), "settled composer releases its shared animation registration")
	require.Empty(t, scheduler.pending, "no idle scheduler chain survives completion or cancellation")
	require.Nil(t, root.ar.Continue())
}

func TestComposerSendHoldsLongerBeforeEasing(t *testing.T) {
	require.Equal(t, 500*time.Millisecond, editorShrinkDelay)
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeyEnter, Mod: tea.ModAlt}} {
		root, scheduler := composerShrinkRoot(t, false)
		root.editor.SetValue(strings.Repeat("row\n", 10))
		root.resizeAll()
		root.Update(key)
		require.Empty(t, root.editor.Value())
		assertComposerDelayedShrink(t, root, scheduler, 11, 1)
		assertComposerSchedulerIdle(t, root, scheduler)
	}
}

func TestComposerHistoryArrowsAnimateGrowthAndShrink(t *testing.T) {
	long := strings.Repeat("row\n", 10)
	for _, lean := range []bool{false, true} {
		for _, key := range []rune{tea.KeyUp, tea.KeyDown} {
			root, scheduler := composerShrinkRoot(t, lean)
			root.history.Messages = []string{"short", long, "short"}
			if key == tea.KeyUp {
				root.history.SetCurrent(2)
			} else {
				root.history.SetCurrent(0)
			}
			root.Update(tea.KeyPressMsg{Code: key})
			require.Equal(t, long, root.editor.Value())
			assertComposerHistoryGrowth(t, root, scheduler, 1, 11)
			root.Update(tea.KeyPressMsg{Code: key})
			require.Equal(t, "short", root.editor.Value())
			assertComposerDelayedShrink(t, root, scheduler, 11, 1)
			assertComposerSchedulerIdle(t, root, scheduler)
		}
	}
}

func TestComposerHistoryRapidRetargetAndTypingInterruption(t *testing.T) {
	root, scheduler := composerShrinkRoot(t, false)
	long := strings.Repeat("row\n", 10)
	root.history.Messages = []string{"short", long}
	root.history.SetCurrent(2)
	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	scheduler.step(t, root)
	from := root.editorHeight
	require.Greater(t, from, 1)
	require.Less(t, from, 11)
	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, from, root.editorHeight, "rapid history shrink starts at the painted height")
	require.Equal(t, 1, root.editorHeightTarget)
	require.Equal(t, int32(1), root.ar.ActiveCount(), "retargeting reuses the single composer lease")
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assertComposerHistoryGrowth(t, root, scheduler, from, 11)
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Empty(t, root.editor.Value())
	assertComposerDelayedShrink(t, root, scheduler, 11, 1)
	assertComposerSchedulerIdle(t, root, scheduler)

	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	scheduler.step(t, root)
	root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	require.Equal(t, 11, root.editorHeight, "typing immediately exposes all draft rows, even without changing line count")
	require.False(t, root.editorHeightMotion.Running())
	root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	require.Equal(t, 12, root.editorHeight, "ordinary newline growth stays immediate")
	before := root.editor.Value()
	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, before, root.editor.Value(), "arrows in a typed draft only move the cursor")
	require.False(t, root.editorHeightMotion.Running())
	assertComposerSchedulerIdle(t, root, scheduler)
}

func TestComposerHistoryGrowthYieldsToManualResizeAndRestoredDraft(t *testing.T) {
	for _, manual := range []bool{false, true} {
		root, scheduler := composerShrinkRoot(t, false)
		root.history.Messages = []string{strings.Repeat("row\n", 10)}
		root.history.SetCurrent(1)
		root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		scheduler.step(t, root)
		if manual {
			root.handleEditorResize(root.composerLayout().separatorTop - 2)
			require.Positive(t, root.manualEditorHeight)
		} else {
			root.Update(messages.RestorePendingMessagesMsg{Content: strings.Repeat("new\n", 11)})
			require.Equal(t, 12, root.editorHeight, "restoring another draft cannot inherit history growth mode")
		}
		require.False(t, root.editorHeightMotion.Running())
		assertComposerSchedulerIdle(t, root, scheduler)
	}
}
