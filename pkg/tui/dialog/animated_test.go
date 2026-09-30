package dialog

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type lifecycleDialog struct {
	view     string
	updates  []tea.Msg
	cleaned  int
	row, col int
}

func (d *lifecycleDialog) Init() tea.Cmd { return nil }
func (d *lifecycleDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	d.updates = append(d.updates, msg)
	return d, nil
}
func (d *lifecycleDialog) View() string             { return d.view }
func (d *lifecycleDialog) SetSize(_, _ int) tea.Cmd { return nil }
func (d *lifecycleDialog) Position() (int, int)     { return d.row, d.col }
func (d *lifecycleDialog) Cleanup()                 { d.cleaned++ }

func TestAnimatedManagerCloseLifecycle(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	dlg := &lifecycleDialog{view: "abc\nx", row: 2, col: 3}
	_, cmd := mgr.handleOpen(OpenDialogMsg{Model: dlg})
	require.Nil(t, cmd, "transition start only registers")
	assert.True(t, mgr.Open())
	assert.True(t, mgr.HasActiveDialog())
	assert.Equal(t, int32(1), runtime.ActiveCount())

	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))
	assert.False(t, mgr.stack[0].opening())

	_, cmd = mgr.Update(CloseDialogMsg{})
	require.Nil(t, cmd, "transition start only registers")
	assert.True(t, mgr.Open(), "closing entries remain rendered")
	assert.True(t, mgr.Closing())
	assert.False(t, mgr.HasActiveDialog())
	assert.Nil(t, mgr.TopDialog())

	before := len(dlg.updates)
	inputs := []tea.Msg{
		tea.KeyPressMsg{},
		tea.KeyReleaseMsg{},
		tea.PasteMsg{Content: "must not reach the closing dialog"},
		tea.PasteStartMsg{},
		tea.PasteEndMsg{},
		tea.MouseClickMsg{},
		tea.MouseMotionMsg{},
		tea.MouseReleaseMsg{},
		tea.MouseWheelMsg{},
		messages.WheelCoalescedMsg{Delta: 3},
	}
	for _, input := range inputs {
		mgr.Update(input)
	}
	assert.Len(t, dlg.updates, before, "closing dialog suppresses all user input")

	result := asyncDialogResultMsg{value: "ready"}
	mgr.Update(result)
	require.Len(t, dlg.updates, before+1)
	assert.Equal(t, result, dlg.updates[before], "async results continue during close")

	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration+dialogCloseDuration))
	assert.False(t, mgr.Open())
	assert.Equal(t, 1, dlg.cleaned)
	assert.Equal(t, int32(0), runtime.ActiveCount())
}

type asyncDialogResultMsg struct {
	value string
}

func TestIsUserInputMsg(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.Msg
	}{
		{name: "key press", msg: tea.KeyPressMsg{}},
		{name: "key release", msg: tea.KeyReleaseMsg{}},
		{name: "paste", msg: tea.PasteMsg{}},
		{name: "paste start", msg: tea.PasteStartMsg{}},
		{name: "paste end", msg: tea.PasteEndMsg{}},
		{name: "mouse click", msg: tea.MouseClickMsg{}},
		{name: "mouse motion", msg: tea.MouseMotionMsg{}},
		{name: "mouse release", msg: tea.MouseReleaseMsg{}},
		{name: "mouse wheel", msg: tea.MouseWheelMsg{}},
		{name: "coalesced wheel", msg: messages.WheelCoalescedMsg{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, isUserInputMsg(tt.msg))
		})
	}

	assert.False(t, isUserInputMsg(asyncDialogResultMsg{}))
	assert.False(t, isUserInputMsg(animation.TickMsg{}))
}

func TestAnimatedManagerExitConfirmationAcceptsCriticalQuitDuringOpening(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	mgr.handleOpen(OpenDialogMsg{Model: NewExitConfirmationDialog()})
	require.True(t, mgr.stack[0].opening())

	_, cmd := mgr.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	assert.Contains(t, msgs, ExitConfirmedMsg{}, "critical quit must be forwarded during opening")
	assert.Contains(t, msgs, CloseDialogMsg{}, "confirmation keeps its normal close transaction")
}

func TestAnimatedManagerPointerSuppressedOnlyDuringTransitions(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	dlg := &lifecycleDialog{view: "dialog"}
	mgr.handleOpen(OpenDialogMsg{Model: dlg})
	baseline := len(dlg.updates)

	mgr.Update(tea.MouseWheelMsg{})
	assert.Len(t, dlg.updates, baseline)

	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))
	baseline = len(dlg.updates)
	mgr.Update(tea.MouseWheelMsg{})
	assert.Len(t, dlg.updates, baseline+1)

	mgr.Update(CloseDialogMsg{})
	mgr.Update(tea.MouseWheelMsg{})
	assert.Len(t, dlg.updates, baseline+1)
}

func TestAnimatedManagerHideReopenPreservesEntry(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	dlg := &lifecycleDialog{view: "dialog"}
	event := &struct{ ID int }{ID: 1}
	mgr.handleOpen(OpenDialogMsg{Model: dlg, OriginatingEvent: event})
	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))

	mgr.handleHide()
	require.True(t, mgr.Closing())
	mgr.stack[0].offsetX = 7

	newEvent := &struct{ ID int }{ID: 2}
	mgr.handleOpen(OpenDialogMsg{Model: dlg, OriginatingEvent: newEvent})
	require.Len(t, mgr.stack, 1)
	assert.False(t, mgr.Closing())
	assert.Same(t, dlg, mgr.TopDialog())
	assert.Same(t, newEvent, mgr.TopBackgroundEvent())
	assert.Equal(t, 7, mgr.stack[0].offsetX)
	assert.Zero(t, dlg.cleaned)
}

func TestAnimatedManagerCloseAllCancelsAndCleans(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime).(*manager)
	mgr.SetSize(80, 24)
	first := &lifecycleDialog{view: "one"}
	second := &lifecycleDialog{view: "two"}
	mgr.handleOpen(OpenDialogMsg{Model: first})
	mgr.handleOpen(OpenDialogMsg{Model: second})
	require.Equal(t, int32(2), runtime.ActiveCount())

	mgr.handleCloseAll()
	assert.False(t, mgr.Open())
	assert.Equal(t, 1, first.cleaned)
	assert.Equal(t, 1, second.cleaned)
	assert.Equal(t, int32(0), runtime.ActiveCount())
}

func TestAnimatedManagerCleanupResetsOwnedState(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := New(runtime)
	mgr.SetSize(80, 24)
	first := &lifecycleDialog{view: "one"}
	second := &lifecycleDialog{view: "two"}
	mgr.Update(OpenDialogMsg{Model: first})
	mgr.Update(OpenDialogMsg{Model: second})
	require.Equal(t, int32(2), runtime.ActiveCount())

	mgr.Cleanup()
	assert.False(t, mgr.Open())
	assert.False(t, mgr.Closing())
	assert.Equal(t, 1, first.cleaned)
	assert.Equal(t, 1, second.cleaned)
	assert.Equal(t, int32(0), runtime.ActiveCount())

	mgr.Cleanup()
	assert.Equal(t, 1, first.cleaned, "cleanup is safe after reset")
	assert.Equal(t, 1, second.cleaned, "cleanup is safe after reset")
}

func TestAnimatedDialogIntermediateFrameSynchronizesFadeAndHeight(t *testing.T) {
	view := "\x1b[38;2;255;0;0;48;2;0;0;255m界e\u0301xx\x1b[0m\n\x1b[38;2;0;255;0msecond\x1b[0m"
	d := &lifecycleDialog{view: view}
	a := &animatedDialog{dialog: d, renderAlpha: 0.5, renderWidth: lipgloss.Width(view), renderHeight: 1}
	got := a.view()
	assert.Equal(t, lipgloss.Width(view), lipgloss.Width(got), "ANSI first/intermediate frame retains final width")
	assert.Equal(t, 1, lipgloss.Height(got))
	assert.Contains(t, got, "38;2;")
	assert.NotEqual(t, view, got, "one shared intermediate state must fade and crop height")
}

func TestAnimatedDialogFramesRemainCenteredAtFinalWidth(t *testing.T) {
	d := &lifecycleDialog{view: "0123456789\nabcdefghij\nABCDEFGHIJ", row: 8, col: 15}
	a := &animatedDialog{dialog: d, renderAlpha: 1, renderWidth: 10, renderHeight: 1, targetWidth: 10}

	row, col := a.position(40, 20)
	assert.Equal(t, 9, row)
	assert.Equal(t, 15, col)
	assert.Equal(t, 10, lipgloss.Width(a.view()))

	// Retargeted and close frames use the same horizontal origin; only their
	// height changes around the content center.
	a.renderHeight = 2
	row, col = a.position(40, 20)
	assert.Equal(t, 9, row)
	assert.Equal(t, 15, col)

	a.closing = true
	a.renderHeight = 1
	row, col = a.position(40, 20)
	assert.Equal(t, 9, row)
	assert.Equal(t, 15, col)
}

func TestAnimatedManagerLayerGeometryUsesFinalWidthAndCenteredHeight(t *testing.T) {
	runtime := newDialogRuntime()
	mgr := &manager{runtime: runtime, width: 40, height: 20}
	d := &lifecycleDialog{view: "0123456789\nabcdefghij\nABCDEFGHIJ", row: 8, col: 15}
	mgr.handleOpen(OpenDialogMsg{Model: d})
	require.Len(t, mgr.stack, 1)

	layer := mgr.GetLayers()[0]
	assert.Equal(t, 10, layer.Width(), "root layer is final dialog width on frame zero")
	assert.Equal(t, 15, layer.GetX(), "root layer keeps the final horizontal center")
	assert.Equal(t, 9, layer.GetY(), "one-row opening frame is vertically centered")

	mgr.stack[0].renderHeight = 2
	layer = mgr.GetLayers()[0]
	assert.Equal(t, 10, layer.Width())
	assert.Equal(t, 15, layer.GetX())
	assert.Equal(t, 9, layer.GetY(), "retarget frame remains centered")
}

func TestToolConfirmationManagerCompactBoundsAcrossOpenFrames(t *testing.T) {
	const viewportWidth, viewportHeight = 165, 47
	runtime := newDialogRuntime()
	mgr := &manager{runtime: runtime, width: viewportWidth, height: viewportHeight}
	dialog := NewToolConfirmationDialog(runtime, realShellLSConfirmationEvent(), &service.SessionState{})
	mgr.handleOpen(OpenDialogMsg{Model: dialog})
	require.Len(t, mgr.stack, 1)

	targetWidth := lipgloss.Width(dialog.View())
	targetHeight := lipgloss.Height(dialog.View())
	choices := dialog.(*toolConfirmationDialog)
	require.Len(t, choices.actions, 6, "each authorization policy has its own described block")
	require.Less(t, targetHeight, viewportHeight, "a short call remains content-sized with described choices")
	require.LessOrEqual(t, targetWidth, viewportWidth)
	require.LessOrEqual(t, targetHeight, viewportHeight)
	assertManagerFrameBounds(t, mgr, targetWidth, 1)

	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration/2))
	intermediateHeight := mgr.stack[0].renderHeight
	assert.Greater(t, intermediateHeight, 1)
	assert.Less(t, intermediateHeight, targetHeight)
	assertManagerFrameBounds(t, mgr, targetWidth, intermediateHeight)

	mgr.handleTick(advanceDialog(runtime, runtime.Continue(), dialogOpenDuration))
	assertManagerFrameBounds(t, mgr, targetWidth, targetHeight)
}

func assertManagerFrameBounds(t *testing.T, mgr *manager, wantWidth, wantHeight int) {
	t.Helper()
	layer := mgr.GetLayers()[0]
	assert.Equal(t, wantWidth, layer.Width(), "width is final from the first frame")
	assert.Equal(t, wantHeight, layer.Height())
	assert.Equal(t, (mgr.width-wantWidth)/2, layer.GetX())
	assert.Equal(t, (mgr.height-wantHeight)/2, layer.GetY(), "each height-only frame stays centered")
}

func TestAnimatedDialogCloseReopenReversalKeepsWidth(t *testing.T) {
	runtime := newDialogRuntime()
	d := &lifecycleDialog{view: "0123456789\nabcdefghij\nABCDEFGHIJ"}
	a, cmd := newAnimatedDialog(runtime, d, 80, 24)
	require.Nil(t, cmd, "transition start only registers")
	assert.Equal(t, 10, a.renderWidth)

	a.renderAlpha, a.renderHeight = 1, 3
	cmd = a.startClose(true)
	assert.Nil(t, cmd, "close reversal reuses the opening transition lease")
	assert.Equal(t, 10, a.renderWidth)
	assert.Equal(t, 10, a.targetWidth)

	cmd = a.reopen(80, 24)
	assert.Nil(t, cmd, "reversal reuses the transition outstanding runtime lease")
	assert.Equal(t, int32(1), runtime.ActiveCount(), "reversal must not register a duplicate lease")
	assert.False(t, a.closing)
	assert.Equal(t, 10, a.renderWidth)
	assert.Equal(t, 10, a.targetWidth)
}

type dialogLifecycleFixture struct {
	name string
	new  func(*animation.Runtime) Dialog
}

func TestSharedDialogLifecycleFixtures(t *testing.T) {
	fixtures := []dialogLifecycleFixture{
		{
			name: "shared wrapper probe",
			new: func(*animation.Runtime) Dialog {
				return &lifecycleDialog{view: "abcdefghijkl\nsecond line\nthird line\nfourth line\nfifth line\nsixth line\nseventh line\neighth line"}
			},
		},
		{
			name: "tool-call confirmation",
			new: func(runtime *animation.Runtime) Dialog {
				return NewToolConfirmationDialog(runtime, newConfirmationEvent(nil), &service.SessionState{})
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const viewportWidth, viewportHeight = 120, 40
				runtime := newDialogRuntime()
				dialog := fixture.new(runtime)
				mgr := &manager{runtime: runtime, width: viewportWidth, height: viewportHeight}
				_, openCmd := mgr.handleOpen(OpenDialogMsg{Model: dialog})
				require.Nil(t, openCmd, "opening registers with the owner")
				require.Len(t, mgr.stack, 1)

				fullView := dialog.View()
				contentWidth, contentHeight := lipgloss.Width(fullView), lipgloss.Height(fullView)
				animated := mgr.stack[0].animatedDialog
				tickCmd := runtime.Continue()
				require.NotNil(t, tickCmd)
				assert.Equal(t, min(contentWidth, viewportWidth), animated.targetWidth, "target width is measured from rendered content")
				assert.Equal(t, min(contentHeight, viewportHeight), animated.targetHeight, "target height is measured from rendered content")
				assert.Equal(t, animated.targetWidth, animated.renderWidth, "first open frame uses final width")
				assert.Equal(t, 1, animated.renderHeight)
				assert.Zero(t, animated.opacity())
				assert.NotEqual(t, animated.targetHeight, animated.renderHeight, "first open frame must not flash at target height")

				progress := tickSharedDialog(t, runtime, &tickCmd, animated, viewportWidth, viewportHeight)
				eased := animation.EaseOutCubic(progress)
				t.Logf("open target=%dx%d first=%dx%d progress=%.4f intermediate=%dx%d alpha=%.4f", animated.targetWidth, animated.targetHeight, animated.fromWidth, animated.fromHeight, progress, animated.renderWidth, animated.renderHeight, animated.opacity())
				assert.InDelta(t, eased, animated.opacity(), 1e-9, "fade uses the shared transition progress")
				assert.Equal(t, animated.targetWidth, animated.renderWidth, "open animates height only")
				assert.Equal(t, interpolateDialogBound(animated.fromHeight, animated.targetHeight, eased), animated.renderHeight)
				assert.Less(t, animated.renderHeight, animated.targetHeight, "intermediate open frame is not full height")

				finishSharedDialogTransition(t, runtime, &tickCmd, animated, viewportWidth, viewportHeight)
				assert.Equal(t, contentWidth, lipgloss.Width(fullView))
				assert.Equal(t, animated.targetWidth, animated.renderWidth, "settled width matches measured content")
				assert.Equal(t, animated.targetHeight, animated.renderHeight, "settled height matches measured content")
				assert.InEpsilon(t, 1.0, animated.opacity(), 0.0001)

				clampedWidth := max(2, animated.targetWidth-7)
				clampedHeight := max(2, animated.targetHeight-3)
				require.Nil(t, animated.retarget("viewport-test", clampedWidth, clampedHeight))
				tickCmd = runtime.Continue()
				require.NotNil(t, tickCmd)
				finishSharedDialogTransition(t, runtime, &tickCmd, animated, clampedWidth, clampedHeight)
				assert.Equal(t, min(contentWidth, clampedWidth), animated.renderWidth, "viewport retarget applies final width immediately")
				assert.Equal(t, min(contentHeight, clampedHeight), animated.renderHeight, "viewport retarget clamps measured height")

				closeWidth, closeHeight := animated.renderWidth, animated.renderHeight
				require.Nil(t, animated.startClose(false))
				tickCmd = runtime.Continue()
				require.NotNil(t, tickCmd)
				progress = tickSharedDialog(t, runtime, &tickCmd, animated, clampedWidth, clampedHeight)
				eased = animation.EaseOutCubic(progress)
				t.Logf("close from=%dx%d progress=%.4f intermediate=%dx%d alpha=%.4f", closeWidth, closeHeight, progress, animated.renderWidth, animated.renderHeight, animated.opacity())
				assert.InDelta(t, 1-eased, animated.opacity(), 1e-9, "reverse fade uses the shared transition progress")
				assert.Equal(t, closeWidth, animated.renderWidth, "close preserves final width while height collapses")
				assert.Equal(t, interpolateDialogBound(closeHeight, 0, eased), animated.renderHeight)
				assert.Less(t, animated.renderHeight, closeHeight)

				finishSharedDialogTransition(t, runtime, &tickCmd, animated, clampedWidth, clampedHeight)
				mgr.width, mgr.height = clampedWidth, clampedHeight
				mgr.handleTick(animation.TickMsg{})
				assert.Empty(t, mgr.stack, "completed close removes the shared wrapper entry")
				assert.Equal(t, int32(0), runtime.ActiveCount(), "completed lifecycle is idle")

				if fixture.name == "tool-call confirmation" {
					assert.True(t, dialogClosable(dialog), "close chrome follows the correlated safe-reject route")
					assert.Nil(t, dialog.(OutsideClickDismisser).OutsideClickDismissCmd())
				}
			})
		})
	}
}

func tickSharedDialog(t *testing.T, runtime *animation.Runtime, cmd *tea.Cmd, animated *animatedDialog, width, height int) float64 {
	t.Helper()
	require.NotNil(t, *cmd)
	msg, ok := (*cmd)().(animation.TickMsg)
	require.True(t, ok)
	_, accepted := runtime.Accept(msg)
	require.True(t, accepted)
	animated.tick("test-tick", width, height)
	progress := animated.anim.Progress()
	*cmd = runtime.Continue()
	return progress
}

func finishSharedDialogTransition(t *testing.T, runtime *animation.Runtime, cmd *tea.Cmd, animated *animatedDialog, width, height int) {
	t.Helper()
	for animated.anim.Running() {
		tickSharedDialog(t, runtime, cmd, animated, width, height)
	}
}

func interpolateDialogBound(from, to int, progress float64) int {
	value := float64(from) + float64(to-from)*progress
	if value >= 0 {
		return int(value + 0.5)
	}
	return -int(-value + 0.5)
}

func TestSameDialogResizeUsesSymmetricInteriorComposition(t *testing.T) {
	r := newDialogRuntime()
	small := strings.Join([]string{"TOP─────────", "TITLE       ", "row one     ", "BOTTOM──────"}, "\n")
	large := strings.Join([]string{"TOP─────────", "TITLE       ", "row one     ", "row two     ", "row three   ", "row four    ", "row five    ", "row six     ", "row seven   ", "row eight   ", "row nine    ", "BOTTOM──────"}, "\n")
	d := &lifecycleDialog{view: small}
	a, tickCmd := newAnimatedDialog(r, d, 80, 30)
	require.Nil(t, tickCmd)
	tickCmd = r.Continue()
	require.Less(t, a.opacity(), 1.0, "initial opening retains its fade")
	a.tick("open", 80, 30)
	advance := func(duration time.Duration) {
		start := r.Now()
		for r.Now()-start < duration {
			acceptedDialogTick(r, tickCmd)
			a.tick("frame", 80, 30)
			tickCmd = r.Continue()
		}
	}
	advance(dialogOpenDuration)
	require.InDelta(t, 1.0, a.opacity(), 0)
	d.view = large
	require.Nil(t, a.retarget("grow", 80, 30))
	tickCmd = r.Continue()
	require.NotNil(t, tickCmd)
	require.True(t, a.resizing)
	grown := []int{a.renderHeight}
	for range 4 {
		advance(2 * animation.TickRate)
		grown = append(grown, a.renderHeight)
		require.InDelta(t, 1.0, a.opacity(), 0)
		view := strings.Split(a.view(), "\n")
		require.True(t, strings.HasPrefix(view[0], "TOP"))
		require.True(t, strings.HasPrefix(view[len(view)-1], "BOTTOM"))
	}
	for a.anim.Running() {
		advance(animation.TickRate)
	}
	require.Equal(t, 12, a.renderHeight)
	d.view = small
	require.Nil(t, a.retarget("shrink", 80, 30))
	tickCmd = r.Continue()
	require.NotNil(t, tickCmd)
	shrunk := []int{a.renderHeight}
	for range 4 {
		advance(2 * animation.TickRate)
		shrunk = append(shrunk, a.renderHeight)
		require.InDelta(t, 1.0, a.opacity(), 0)
		view := strings.Split(a.view(), "\n")
		require.True(t, strings.HasPrefix(view[0], "TOP"))
		require.True(t, strings.HasPrefix(view[len(view)-1], "BOTTOM"))
	}
	for i := range grown {
		require.InDelta(t, 12-4, grown[i]-4+shrunk[i]-4, 1, "matched elapsed grow/shrink use equal linear progress")
	}
	for a.anim.Running() {
		advance(animation.TickRate)
	}
	require.Equal(t, 4, a.renderHeight)
	require.Nil(t, a.startClose(false))
	tickCmd = r.Continue()
	require.NotNil(t, tickCmd)
	require.False(t, a.resizing)
	advance(animation.TickRate)
	require.Less(t, a.opacity(), 1.0, "closing alone adds the fade")
	for a.anim.Running() {
		advance(animation.TickRate)
	}
	require.Zero(t, r.ActiveCount())
}

func TestResizeReversalSamplesCurrentBoundsWithoutReopeningFade(t *testing.T) {
	r := newDialogRuntime()
	d := &lifecycleDialog{view: strings.Repeat("wide row\n", 3) + "bottom"}
	a, tickCmd := newAnimatedDialog(r, d, 40, 20)
	require.Nil(t, tickCmd)
	tickCmd = r.Continue()
	for a.anim.Running() {
		acceptedDialogTick(r, tickCmd)
		a.tick("open", 40, 20)
		tickCmd = r.Continue()
	}
	d.view = strings.Repeat("wide row\n", 15) + "bottom"
	require.Nil(t, a.retarget("grow", 40, 20))
	tickCmd = r.Continue()
	for range 3 {
		acceptedDialogTick(r, tickCmd)
		a.tick("grow", 40, 20)
		tickCmd = r.Continue()
	}
	before := a.renderHeight
	d.view = "wide row\nbottom"
	a.retarget("reverse", 40, 20)
	require.Equal(t, before, a.fromHeight)
	require.Equal(t, before, a.renderHeight)
	require.InDelta(t, 1.0, a.fromAlpha, 0)
	require.InDelta(t, 1.0, a.targetAlpha, 0)
	require.Equal(t, int32(1), r.ActiveCount())
	a.cancel()
	require.Zero(t, r.ActiveCount())
}

func TestResizeCompositionStaysBoundedAndSuppressesTransientActionHits(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(60, 20)
	d := NewSnapshotsDialog([]int{1}).(*snapshotsDialog)
	_, openCmd := mgr.handleOpen(OpenDialogMsg{Model: d})
	require.Nil(t, openCmd, "opening registers with the owner")
	tickCmd := r.Continue()
	for mgr.stack[0].anim.Running() {
		tick := acceptedDialogTick(r, tickCmd)
		mgr.handleTick(tick)
		tickCmd = r.Continue()
	}
	d.fileCounts = make([]int, 30)
	_, updateCmd := mgr.Update(tea.WindowSizeMsg{Width: 16, Height: 8})
	require.Nil(t, updateCmd, "resize registers with the owner")
	entry := &mgr.stack[0]
	require.True(t, entry.resizing)
	require.True(t, mgr.pointerSuppressed())
	row, col := d.Position()
	dl := NewDialogLayout(d.View(), row, col)
	for y := row; y < row+dl.Height; y++ {
		for x := col; x < col+dl.Width; x++ {
			_, cmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y})
			require.Nil(t, cmd, "resize frames cannot activate clipped or offscreen actions")
		}
	}
	frame := mgr.View()
	require.LessOrEqual(t, lipgloss.Width(frame), 16)
	require.LessOrEqual(t, lipgloss.Height(frame), 8)
	bounds := entry.renderHeight
	alpha := entry.opacity()
	mgr.Update(messages.ThemeChangedMsg{})
	require.Equal(t, bounds, entry.renderHeight)
	require.InDelta(t, alpha, entry.opacity(), 0, "theme refresh does not restart opening fade")
	mgr.Cleanup()
	require.Zero(t, r.ActiveCount())
}
