package dialog

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	dialogOpenDuration   = 132 * time.Millisecond
	dialogCloseDuration  = 132 * time.Millisecond
	dialogResizeDuration = 180 * time.Millisecond
)

// DialogLifecycleException is an explicit, reviewable opt-out from the shared
// visual lifecycle. It is reserved for dialogs that cannot be safely cropped.
//
//nolint:revive // Explicit name distinguishes lifecycle exceptions from other dialog exceptions.
type DialogLifecycleException interface {
	DisableDialogLifecycleAnimation() bool
}

// animatedDialog owns the visual lifecycle of one stack entry. A single
// transition drives alpha and rendered height. Width is always the final
// measured width, preventing narrow first frames while height opens, closes,
// resizes, and reverses around the dialog's center.
type dialogBoundsEvent struct {
	At                   time.Time
	Cause                string
	MeasuredWidth        int
	MeasuredHeight       int
	PreviousTargetWidth  int
	PreviousTargetHeight int
	Retargeted           bool
}

type animatedDialog struct {
	dialog Dialog
	anim   animation.Transition

	fromAlpha, targetAlpha    float64
	fromWidth, targetWidth    int
	fromHeight, targetHeight  int
	renderAlpha               float64
	renderWidth, renderHeight int
	closing, hiding           bool
	disabled                  bool
	lastBoundsEvent           dialogBoundsEvent
	boundsMeasurementCount    int
	cachedView                string
	viewTheme                 uint64
	viewValid                 bool
}

func newAnimatedDialog(runtime *animation.Runtime, dialog Dialog, maxWidth, maxHeight int) (*animatedDialog, tea.Cmd) {
	a := &animatedDialog{dialog: dialog, anim: runtime.Transition()}
	if exception, ok := dialog.(DialogLifecycleException); ok {
		a.disabled = exception.DisableDialogLifecycleAnimation()
	}
	w, h := a.measureBounds("open", maxWidth, maxHeight, true)
	a.targetAlpha, a.targetWidth, a.targetHeight = 1, w, h
	if a.disabled || w == 0 || h == 0 {
		a.anim.Cancel()
		a.renderAlpha, a.renderWidth, a.renderHeight = 1, w, h
		return a, nil
	}
	a.renderWidth, a.renderHeight = w, min(1, h)
	return a, a.anim.Start(dialogOpenDuration, animation.EaseOutCubic)
}

func (a *animatedDialog) desiredBounds(maxWidth, maxHeight int) (int, int) {
	view := a.intrinsicView()
	return min(max(0, lipgloss.Width(view)), max(0, maxWidth)), min(max(0, lipgloss.Height(view)), max(0, maxHeight))
}

func (a *animatedDialog) measureBounds(cause string, maxWidth, maxHeight int, opening bool) (int, int) {
	w, h := a.desiredBounds(maxWidth, maxHeight)
	event := dialogBoundsEvent{
		At: time.Now(), Cause: cause, MeasuredWidth: w, MeasuredHeight: h,
		PreviousTargetWidth: a.targetWidth, PreviousTargetHeight: a.targetHeight,
		Retargeted: opening || w != a.targetWidth || h != a.targetHeight,
	}
	a.lastBoundsEvent = event
	a.boundsMeasurementCount++
	slog.Debug("Dialog intrinsic bounds measured",
		"dialog", stringType(a.dialog), "cause", cause, "animation_time", event.At,
		"width", w, "height", h, "previous_width", event.PreviousTargetWidth,
		"previous_height", event.PreviousTargetHeight, "retargeted", event.Retargeted)
	return w, h
}

func stringType(v any) string {
	return fmt.Sprintf("%T", v)
}

func (a *animatedDialog) sample() {
	if a.disabled || !a.anim.Running() {
		return
	}
	a.anim.Tick()
	a.renderAlpha = a.fromAlpha + (a.targetAlpha-a.fromAlpha)*a.anim.Value()
	a.renderWidth = a.targetWidth
	a.renderHeight = a.anim.Lerp(a.fromHeight, a.targetHeight)
	if !a.anim.Running() {
		a.renderAlpha, a.renderWidth, a.renderHeight = a.targetAlpha, a.targetWidth, a.targetHeight
	}
}

func (a *animatedDialog) retarget(cause string, maxWidth, maxHeight int) tea.Cmd {
	if a.closing {
		if maxWidth <= 0 || maxHeight <= 0 {
			a.anim.Cancel()
			a.renderAlpha, a.renderWidth, a.renderHeight = 0, 0, 0
		}
		return nil
	}
	a.invalidateView()
	w, h := a.measureBounds(cause, maxWidth, maxHeight, false)
	if a.disabled || w == 0 || h == 0 {
		a.anim.Cancel()
		a.targetAlpha, a.renderAlpha = 1, 1
		a.targetWidth, a.targetHeight = w, h
		a.renderWidth, a.renderHeight = w, h
		return nil
	}
	if w == a.targetWidth && h == a.targetHeight {
		return nil
	}
	a.sample()
	a.fromAlpha, a.fromHeight = a.renderAlpha, a.renderHeight
	a.fromWidth = w
	a.targetAlpha, a.targetWidth, a.targetHeight = 1, w, h
	a.renderWidth = w
	return a.anim.Start(dialogResizeDuration, animation.Linear)
}

func (a *animatedDialog) reopen(maxWidth, maxHeight int) tea.Cmd {
	a.sample()
	a.closing, a.hiding = false, false
	w, h := a.measureBounds("reopen", maxWidth, maxHeight, true)
	a.fromAlpha, a.fromHeight = a.renderAlpha, a.renderHeight
	a.fromWidth = w
	a.targetAlpha, a.targetWidth, a.targetHeight = 1, w, h
	a.renderWidth = w
	if a.disabled || w == 0 || h == 0 {
		a.anim.Cancel()
		a.renderAlpha, a.renderWidth, a.renderHeight = 1, w, h
		return nil
	}
	return a.anim.Start(dialogOpenDuration, animation.EaseOutCubic)
}

func (a *animatedDialog) startClose(hiding bool) tea.Cmd {
	if a.closing {
		if hiding {
			a.hiding = true
		}
		return nil
	}
	visible := a.targetWidth > 0 && a.targetHeight > 0
	a.sample()
	a.closing, a.hiding = true, hiding
	a.fromAlpha, a.fromHeight = a.renderAlpha, a.renderHeight
	a.fromWidth = a.renderWidth
	a.targetAlpha, a.targetHeight = 0, 0
	if a.disabled || !visible {
		a.anim.Cancel()
		a.renderAlpha, a.renderHeight = 0, 0
		return nil
	}
	return a.anim.Start(dialogCloseDuration, animation.EaseOutCubic)
}

//nolint:unparam // Command result is retained for transition protocol symmetry.
func (a *animatedDialog) tick(_ string, _, _ int) (finished bool, cmd tea.Cmd) {
	a.sample()
	return a.closing && !a.anim.Running(), nil
}

func (a *animatedDialog) opacity() float64 { return a.renderAlpha }

func (a *animatedDialog) position(maxWidth, maxHeight int) (row, col int) {
	return CenterPosition(maxWidth, maxHeight, a.renderWidth, a.renderHeight)
}

func (a *animatedDialog) opening() bool { return !a.closing && a.anim.Running() }
func (a *animatedDialog) cancel() {
	if !a.disabled {
		a.anim.Cancel()
	}
}

// view crops vertically around the center of the desired card. The same
// progress that controls height controls ANSI-aware color opacity above.
func (a *animatedDialog) view() string {
	return a.viewWithChrome(false, false)
}

func (a *animatedDialog) viewWithChrome(closable, hovered bool) string {
	view := a.intrinsicView()
	if closable {
		view = renderCloseControl(view, hovered)
	}
	if a.disabled {
		return view
	}
	w, h := max(0, a.renderWidth), max(0, a.renderHeight)
	if w == 0 || h == 0 {
		return ""
	}
	fullH := lipgloss.Height(view)
	lines := strings.Split(view, "\n")
	if h < fullH {
		y := max(0, (fullH-h)/2)
		lines = lines[y:min(len(lines), y+h)]
	} else if h > fullH {
		padded := make([]string, 0, h)
		// Resize padding remains card chrome, not transparent/trimmed full-width blank rows.
		blank := styles.DialogStyle.Padding(0, 2).BorderTop(false).BorderBottom(false).Width(w).Render("")
		padding := make([]string, h-fullH)
		for i := range padding {
			padding[i] = blank
		}
		if len(lines) == 1 {
			padded = append(padded, lines...)
			padded = append(padded, padding...)
		} else {
			padded = append(padded, lines[:len(lines)-1]...)
			padded = append(padded, padding...)
			padded = append(padded, lines[len(lines)-1])
		}
		lines = padded
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "")
		if pad := w - lipgloss.Width(lines[i]); pad > 0 {
			lines[i] += strings.Repeat(" ", pad)
		}
	}
	view = strings.Join(lines, "\n")

	if a.renderAlpha >= 1 {
		return view
	}

	// Fade preserves grapheme widths after the centered crop.
	fc := styles.NewFadeContext()
	lines = strings.Split(view, "\n")
	for i := range lines {
		lines[i] = styles.FadeLineCtx(lines[i], a.renderAlpha, &fc)
	}
	return strings.Join(lines, "\n")
}

// CleanupDialog releases visual resources without answering or cancelling a prompt.
func CleanupDialog(dialog Dialog) {
	if cleanup, ok := dialog.(interface{ Cleanup() }); ok {
		cleanup.Cleanup()
	}
}

func isUserInputMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.KeyReleaseMsg, tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg,
		tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, tea.MouseWheelMsg, messages.WheelCoalescedMsg:
		return true
	default:
		return false
	}
}

func (a *animatedDialog) intrinsicView() string {
	if !a.viewValid || a.viewTheme != styles.ThemeGeneration() {
		a.cachedView = a.dialog.View()
		a.viewValid = true
		a.viewTheme = styles.ThemeGeneration()
	}
	return a.cachedView
}
func (a *animatedDialog) invalidateView() { a.viewValid = false }
