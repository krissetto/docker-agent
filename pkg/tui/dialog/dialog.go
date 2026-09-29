//nolint:gocritic // Dialog command returns intentionally preserve Bubble Tea evaluation shape.
package dialog

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// OpenDialogMsg is sent to open a new dialog.
//
// OriginatingEvent is an optional runtime event whose presence marks the
// dialog as a background dialog. Background dialogs do not block tab
// navigation: tab-switch keys and tab-bar mouse clicks keep working. When
// the user switches away from the tab that opened the dialog, the dialog is
// closed and OriginatingEvent is re-stashed in the supervisor so the same
// prompt is re-displayed when the user returns. Other input (including
// mouse-wheel events) is still routed to the dialog while it is on screen.
type OpenDialogMsg struct {
	Model            Dialog
	OriginatingEvent tea.Msg
}

// ReconcileInteractionsMsg removes only prompts absent from an authoritative head.
type ReconcileInteractionsMsg struct {
	SessionID  string
	Projection *lifecycle.Projection
}

// CloseDialogMsg is sent to close the current (topmost) dialog
type CloseDialogMsg struct{}

// CloseDialogByModelMsg closes only the specified dialog instance, including a buried editor.
type CloseDialogByModelMsg struct{ Model Dialog }

// HideDialogMsg fades the current dialog out without cleaning up its state.
type HideDialogMsg struct{}

// CloseAllDialogsMsg is sent to close all dialogs in the stack
type CloseAllDialogsMsg struct{}

// Broadcastable marks messages the manager delivers to every dialog in the
// stack instead of only the topmost one. Data-refresh messages implement it
// so dialogs buried under another dialog (e.g. the plan browser under its
// detail dialog) stay fresh.
type Broadcastable interface {
	BroadcastToDialogs()
}

// Dialog defines the interface that all dialogs must implement
type Dialog interface {
	layout.Model
	Position() (int, int) // Returns (row, col) for dialog placement
}

// OutsideClickDismisser allows mandatory dialogs to explicitly ignore the
// default outside-left-click cancellation policy. Returning nil keeps the
// dialog open. Outside clicks must never perform an affirmative action.
type OutsideClickDismisser interface {
	OutsideClickDismissCmd() tea.Cmd
}

type SemanticCloser interface {
	CancelDialogCmd() tea.Cmd
}

type ClosePolicy interface{ DialogClosable() bool }

// Manager manages the dialog stack and rendering
type Manager interface {
	layout.Model

	GetLayers() []*lipgloss.Layer
	GetLayerInfos() []styles.LayerInfo
	Open() bool
	HasActiveDialog() bool
	Closing() bool
	// Cleanup immediately cancels animation leases, cleans dialog resources,
	// and resets the manager. Call it before replacing a manager instance.
	Cleanup()
	TopIsExitConfirmation() bool
	// TopIsBackground reports whether the topmost dialog is a background
	// dialog (i.e. it should not block tab navigation).
	TopIsBackground() bool
	// TopBackgroundEvent returns the originating event of the topmost
	// background dialog, or nil if the top dialog is not a background dialog
	// or the dialog stack is empty.
	TopBackgroundEvent() tea.Msg
	// TopDialog returns the topmost dialog instance, or nil if the dialog
	// stack is empty. Used by the app model to stash a background dialog's
	// live state when the user navigates away from the tab that opened it,
	// so the same instance (with any in-progress input) can be re-opened on
	// return.
	TopDialog() Dialog
	// HasDialog reports whether pred matches any dialog in the stack,
	// visiting bottom to top. Unlike TopDialog it also sees dialogs buried
	// under other dialogs, e.g. a plan browser under a help dialog.
	HasDialog(pred func(Dialog) bool) bool
	// TakeVisualDirty reports and clears an explicit pointer-driven visible mutation.
	TakeVisualDirty() bool
}

// dialogEntry pairs a dialog with its drag offset so the two stay in sync.
type dialogEntry struct {
	*animatedDialog

	cancelPending bool
	closeHovered  bool // hover belongs to this stack entry's open lifecycle
	offsetX       int  // accumulated horizontal drag displacement
	offsetY       int  // accumulated vertical drag displacement
	// originatingEvent is the runtime event that caused this dialog to open,
	// when applicable. A non-nil value marks the dialog as a background
	// dialog (see OpenDialogMsg.OriginatingEvent).
	originatingEvent tea.Msg
	interactionKey   lifecycle.InteractionKey
}

// dragState tracks an in-progress drag operation.
type dragState struct {
	active bool
	startX int // screen X where drag began
	startY int // screen Y where drag began
	origDX int // dialog offsetX at drag start
	origDY int // dialog offsetY at drag start
}

// manager implements Manager
type manager struct {
	runtime       *animation.Runtime
	width, height int
	stack         []dialogEntry
	drag          dragState
	projections   map[string]*lifecycle.Projection
	closeHovered  bool //nolint:unused // Retained as lifecycle hover state for concrete dialogs.
	visualDirty   bool
}

// New creates a new dialog component manager
func New(runtime *animation.Runtime) Manager {
	if runtime == nil {
		panic("dialog: nil animation runtime")
	}
	return &manager{runtime: runtime}
}

// Init initializes the dialog component
func (d *manager) Init() tea.Cmd {
	return nil
}

// Update handles messages and updates dialog state
func (d *manager) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// A closing entry remains rendered and modal, but must not receive any
	// terminal input. Non-input messages (for example async results) continue
	// to reach the same dialog instance until its close animation completes.
	if (len(d.stack) == 0 || d.Closing() || d.stack[len(d.stack)-1].cancelPending) && isUserInputMsg(msg) {
		return d, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.width = msg.Width
		d.height = msg.Height
		cmd := d.broadcastToAll(msg)
		return d, cmd

	case messages.ThemeChangedMsg:
		cmd := d.broadcastToAll(msg)
		return d, cmd

	case ReconcileInteractionsMsg:
		if d.projections == nil {
			d.projections = make(map[string]*lifecycle.Projection)
		}
		d.projections[msg.SessionID] = msg.Projection
		d.stack = slices.DeleteFunc(d.stack, func(entry dialogEntry) bool {
			key := entry.interactionKey
			if key.SessionID == msg.SessionID && key.InteractionID != "" && !msg.Projection.HasInteraction(key) {
				entry.cancel()
				CleanupDialog(entry.dialog)
				return true
			}
			return false
		})
		d.drag.active = false
		return d, nil
	case animation.TickMsg:
		return d.handleTick(msg)

	case OpenDialogMsg:
		return d.handleOpen(msg)

	case CloseDialogMsg:
		return d.beginClose(false)
	case CloseDialogByModelMsg:
		return d.closeByModel(msg.Model)

	case HideDialogMsg:
		return d.beginClose(true)

	case ClosePlanDetailMsg:
		return d.handleClosePlanDetail(msg)

	case CloseAllDialogsMsg:
		return d.handleCloseAll()

	case tea.MouseClickMsg:
		if d.pointerSuppressed() {
			return d, nil
		}
		adjusted := d.adjustMouseMsg(msg)
		if msg.Button == tea.MouseLeft {
			if cmd := d.handleOutsideClickDismiss(msg); cmd != nil {
				return d, cmd
			}
			if actions, ok := d.stack[len(d.stack)-1].dialog.(interface {
				ActionKeyAt(x, y int, dl DialogLayout) (tea.KeyPressMsg, bool)
			}); ok {
				e := &d.stack[len(d.stack)-1]
				row, col := e.position(d.width, d.height)
				dl := NewDialogLayout(e.view(), row+e.offsetY, col+e.offsetX)
				if action, hit := actions.ActionKeyAt(msg.X, msg.Y, dl); hit {
					if focus, ok := e.dialog.(interface{ BlurActions() }); ok {
						focus.BlurActions()
					}
					return d, d.forwardToTop(action)
				}
			}
			// The close control gets first refusal; other title clicks drag.
			if d.closeButtonHit(msg.X, msg.Y) {
				return d, d.semanticCloseTop()
			}
			if d.handleDragStart(msg.X, msg.Y) {
				return d, nil
			}
		}
		if msg.Button == tea.MouseLeft {
			if focus, ok := d.stack[len(d.stack)-1].dialog.(interface{ BlurActions() }); ok {
				focus.BlurActions()
			}
		}
		return d, d.forwardToTop(adjusted)

	case tea.MouseMotionMsg:
		top := len(d.stack) - 1
		hovered := d.closeButtonHit(msg.X, msg.Y)
		d.visualDirty = d.visualDirty || hovered != d.stack[top].closeHovered
		d.stack[top].closeHovered = hovered
		if chrome, ok := d.stack[top].dialog.(interface{ SetCloseHover(hovered bool) }); ok {
			chrome.SetCloseHover(hovered)
		}
		if d.pointerSuppressed() {
			return d, nil
		}
		if d.drag.active {
			d.visualDirty = d.handleDragMotion(msg.X, msg.Y) || d.visualDirty
			return d, nil
		}
		cmd := d.forwardToTopWithoutRetarget(d.adjustMouseMsg(msg))
		return d, cmd

	case tea.MouseReleaseMsg:
		if d.pointerSuppressed() {
			d.drag.active = false
			return d, nil
		}
		if d.drag.active {
			d.drag.active = false
			return d, nil
		}
		cmd := d.forwardToTop(d.adjustMouseMsg(msg))
		return d, cmd

	case messages.WheelCoalescedMsg:
		if d.pointerSuppressed() {
			return d, nil
		}
		return d, d.forwardToTopWithoutRetarget(d.adjustMouseMsg(msg))

	case tea.MouseWheelMsg:
		if d.pointerSuppressed() {
			return d, nil
		}
		cmd := d.forwardToTopWithoutRetarget(d.adjustMouseMsg(msg))
		d.takeTopVisualDirty()
		return d, cmd
	case tea.KeyPressMsg:
		// Ctrl-C normally cancels the current dialog. The exit confirmation is
		// the critical exception: a second Ctrl-C confirms exit even while its
		// opening animation is running. Other opening input remains subject to
		// the existing dialog semantics and pointer suppression.
		if key.Matches(msg, core.GetKeys().Quit) {
			if d.TopIsExitConfirmation() {
				return d, d.forwardToTop(msg)
			}
			return d, d.semanticCloseTop()
		}
		if msg.Code == tea.KeyEscape && dialogClosable(d.stack[len(d.stack)-1].dialog) {
			return d, d.semanticCloseTop()
		}
		return d, d.forwardToTop(msg)
	}

	if _, ok := msg.(Broadcastable); ok {
		cmd := d.broadcastToAll(msg)
		return d, cmd
	}

	// Forward non-mouse messages to top dialog
	cmd := d.forwardToTop(msg)
	return d, cmd
}

// View renders all dialogs (used for debugging, actual rendering uses GetLayers)
func (d *manager) View() string {
	if len(d.stack) == 0 {
		return ""
	}
	return d.entryView(&d.stack[len(d.stack)-1])
}

// broadcastToAll sends a message to every dialog in the stack and batches the resulting commands.
func (d *manager) broadcastToAll(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for i := range d.stack {
		u, cmd := d.stack[i].dialog.Update(msg)
		d.stack[i].dialog = u.(Dialog)
		d.stack[i].invalidateView()
		cmds = append(cmds, cmd, d.stack[i].retarget("broadcast:"+stringType(msg), d.width, d.height))
	}
	return tea.Batch(cmds...)
}

// forwardToTop forwards a message to the topmost dialog and returns the resulting command.
func (d *manager) forwardToTop(msg tea.Msg) tea.Cmd {
	return d.forwardToTopWithRetarget(msg, true)
}

func (d *manager) forwardToTopWithoutRetarget(msg tea.Msg) tea.Cmd {
	return d.forwardToTopWithRetarget(msg, false)
}

func (d *manager) forwardToTopWithRetarget(msg tea.Msg, retarget bool) tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	top := len(d.stack) - 1
	if scroller, ok := d.stack[top].dialog.(interface {
		UpdateActionScroll(msg tea.Msg) (bool, tea.Cmd)
	}); ok {
		if handled, cmd := scroller.UpdateActionScroll(msg); handled {
			d.takeTopVisualDirty()
			return cmd
		}
	}
	tick, ticking := msg.(animation.TickMsg)
	wasTickDirty := ticking && tick.Dirty()
	u, cmd := d.stack[top].dialog.Update(msg)
	d.stack[top].dialog = u.(Dialog)
	switch msg.(type) {
	case animation.TickMsg, tea.MouseMotionMsg, tea.MouseWheelMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		// Registered viewports publish actual visual changes below; boundary input stays warm.
	default:
		d.stack[top].invalidateView()
	}
	d.takeTopVisualDirty()
	if ticking && !wasTickDirty && tick.Dirty() {
		d.stack[top].invalidateView()
	}
	if !retarget {
		return cmd
	}
	resizeCmd := d.stack[top].retarget("update:"+stringType(msg), d.width, d.height)
	if resizeCmd == nil {
		return cmd
	}
	if cmd == nil {
		return resizeCmd
	}
	return tea.Batch(cmd, resizeCmd)
}

// titleZoneHeight is the number of rows from the top of a dialog that form
// the draggable title zone: border top + padding top + title line + separator.
const titleZoneHeight = 4

// handleDragStart checks if a mouse click is in the title zone of the topmost
// dialog (border, padding, title text, and separator). If so, it initiates a
// drag operation and returns true.
func (d *manager) handleDragStart(x, y int) bool {
	if len(d.stack) == 0 {
		return false
	}
	top := len(d.stack) - 1
	e := &d.stack[top]

	row, col := e.position(d.width, d.height)
	row += e.offsetY
	col += e.offsetX
	w := e.renderWidth

	// Check horizontal bounds
	if x < col || x >= col+w {
		return false
	}
	// Check vertical bounds: click must be within the title zone
	zoneHeight := titleZoneHeight
	if body, ok := e.dialog.(interface{ BodyScrollBounds() (int, int, int, int) }); ok {
		_, _, _, h := body.BodyScrollBounds()
		if h > 0 {
			zoneHeight = 2
		}
	}
	if body, ok := e.dialog.(interface{ BodyScrollBounds() (int, int, int, int) }); ok {
		_, bodyY, _, h := body.BodyScrollBounds()
		if h > 0 {
			zoneHeight = min(zoneHeight, bodyY+e.offsetY-row)
		}
	}
	if y < row || y >= row+zoneHeight {
		return false
	}

	d.drag = dragState{
		active: true,
		startX: x,
		startY: y,
		origDX: e.offsetX,
		origDY: e.offsetY,
	}
	return true
}

// handleDragMotion updates the drag offset during a drag operation.
func (d *manager) handleDragMotion(x, y int) bool {
	if len(d.stack) == 0 {
		return false
	}
	e := &d.stack[len(d.stack)-1]
	oldX, oldY := e.offsetX, e.offsetY
	row, col := e.position(d.width, d.height)
	view := e.view()
	w, h := lipgloss.Width(view), lipgloss.Height(view)
	proposedX := d.drag.origDX + (x - d.drag.startX)
	proposedY := d.drag.origDY + (y - d.drag.startY)
	maxCol := max(0, d.width-w)
	maxRow := max(0, d.height-h)
	e.offsetX = min(max(proposedX, -col), maxCol-col)
	e.offsetY = min(max(proposedY, -row), maxRow-row)
	return oldX != e.offsetX || oldY != e.offsetY
}

// adjustMouseMsg adjusts mouse coordinates in a message to account for the drag offset
// of the top dialog, so that the dialog's internal hit-testing works correctly.
func (d *manager) adjustMouseMsg(msg tea.Msg) tea.Msg {
	if len(d.stack) == 0 {
		return msg
	}
	e := d.stack[len(d.stack)-1]
	if e.offsetX == 0 && e.offsetY == 0 {
		return msg
	}

	switch m := msg.(type) {
	case tea.MouseClickMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseMotionMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseReleaseMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case messages.WheelCoalescedMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	case tea.MouseWheelMsg:
		m.X -= e.offsetX
		m.Y -= e.offsetY
		return m
	}
	return msg
}

// closeButtonHit reports whether screen coordinates hit the one-cell close
// control on the top border of the topmost dialog.
func (d *manager) closeButtonHit(x, y int) bool {
	if len(d.stack) == 0 || !dialogClosable(d.stack[len(d.stack)-1].dialog) {
		return false
	}
	e := d.stack[len(d.stack)-1]
	row, col := e.position(d.width, d.height)
	row += e.offsetY
	col += e.offsetX
	closeX, closeY, ok := closeControlCell(e.renderWidth, e.renderHeight)
	return ok && x == col+closeX && y == row+closeY
}

func dialogClosable(dialog Dialog) bool {
	if policy, ok := dialog.(ClosePolicy); ok {
		return policy.DialogClosable()
	}
	return true
}

func (d *manager) semanticCloseTop() tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	top := &d.stack[len(d.stack)-1]
	if top.cancelPending || top.closing {
		return nil
	}
	top.cancelPending = true
	closer, ok := top.dialog.(SemanticCloser)
	if !ok {
		return core.CmdHandler(CloseDialogMsg{})
	}
	return closer.CancelDialogCmd()
}

func (d *manager) entryView(e *dialogEntry) string {
	view := e.viewWithChrome(dialogClosable(e.dialog), e.closeHovered)
	return clampRenderedFrame(view, d.width, d.height)
}

func clampRenderedFrame(view string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "")
	}
	return strings.Join(lines, "\n")
}

// handleOutsideClickDismiss applies cancellation on an outside left click
// while preserving each entry's drag offset. Mandatory dialogs may opt out;
// no outside click is allowed to select an affirmative action.
func (d *manager) handleOutsideClickDismiss(msg tea.MouseClickMsg) tea.Cmd {
	if len(d.stack) == 0 {
		return nil
	}
	e := d.stack[len(d.stack)-1]
	row, col := e.position(d.width, d.height)
	row += e.offsetY
	col += e.offsetX
	view := e.view()
	if msg.X >= col && msg.X < col+lipgloss.Width(view) && msg.Y >= row && msg.Y < row+lipgloss.Height(view) {
		return nil
	}
	if dismisser, ok := e.dialog.(OutsideClickDismisser); ok {
		return dismisser.OutsideClickDismissCmd()
	}
	return d.semanticCloseTop()
}

func resetDialogCloseHover(dialog Dialog) {
	if resetter, ok := dialog.(interface{ ResetCloseHover() }); ok {
		resetter.ResetCloseHover()
	}
}

// clearTopPointerState releases only pointer presentation when another dialog occludes this entry.
func (d *manager) clearTopPointerState() {
	d.drag.active = false
	if len(d.stack) == 0 {
		return
	}
	entry := &d.stack[len(d.stack)-1]
	if stopper, ok := entry.dialog.(interface{ StopAnimations() }); ok {
		stopper.StopAnimations()
	}
	changed := entry.closeHovered
	entry.closeHovered = false
	if chrome, ok := entry.dialog.(interface{ SetCloseHover(hovered bool) }); ok {
		chrome.SetCloseHover(false)
	} else {
		resetDialogCloseHover(entry.dialog)
	}
	entry.invalidateView()
	d.takeTopVisualDirty()
	d.visualDirty = d.visualDirty || changed
}

// handleOpen processes dialog opening requests and adds to stack
func (d *manager) handleOpen(msg OpenDialogMsg) (layout.Model, tea.Cmd) {
	key := lifecycle.InteractionIdentity(msg.OriginatingEvent)
	if msg.OriginatingEvent == nil {
		if child, ok := msg.Model.(*multiChoiceDialog); ok {
			if correlation, ok := child.config.Context.(messages.InteractionResponseMsg); ok {
				key = lifecycle.InteractionKey{SessionID: correlation.SessionID, InteractionID: correlation.Response.InteractionID}
			}
		}
	}
	if head, ok := d.projections[key.SessionID]; ok && key.InteractionID != "" && !head.HasInteraction(key) {
		return d, nil
	}
	resetDialogCloseHover(msg.Model)
	// A hidden dialog can be reopened before its close completes. Reuse the
	// existing wrapper so there is one rendered layer and the fade reverses
	// smoothly while preserving offsets and originating metadata.
	for i := len(d.stack) - 1; i >= 0; i-- {
		entry := &d.stack[i]
		if entry.dialog == msg.Model && entry.closing && entry.hiding {
			d.clearTopPointerState()
			entry.originatingEvent = msg.OriginatingEvent
			entry.interactionKey = key
			entry.cancelPending = false
			cmd := entry.reopen(d.width, d.height)
			reopened := *entry
			d.stack = append(slices.Delete(d.stack, i, i+1), reopened)
			return d, cmd
		}
	}

	if msg.OriginatingEvent != nil && key.InteractionID != "" && slices.ContainsFunc(d.stack, func(entry dialogEntry) bool { return lifecycle.InteractionIdentity(entry.originatingEvent) == key }) {
		return d, nil
	}

	d.clearTopPointerState()

	if bindable, ok := msg.Model.(interface {
		BindAnimationRuntime(runtime *animation.Runtime)
	}); ok {
		bindable.BindAnimationRuntime(d.runtime)
	}
	initCmd := msg.Model.Init()

	// Size the concrete dialog before the shared wrapper measures its rendered
	// target. Content-derived dialogs (notably tool confirmation) otherwise
	// begin from their zero-size fallback and retarget on the first tick.
	_, sizeCmd := msg.Model.Update(tea.WindowSizeMsg{
		Width:  d.width,
		Height: d.height,
	})
	animated, animationCmd := newAnimatedDialog(d.runtime, msg.Model, d.width, d.height)
	// The measured initial view already contains preparation changes; consume their dirty baseline.
	if dirty, ok := msg.Model.(interface{ TakeVisualDirty() bool }); ok {
		dirty.TakeVisualDirty()
	}
	d.stack = append(d.stack, dialogEntry{
		animatedDialog:   animated,
		originatingEvent: msg.OriginatingEvent,
		interactionKey:   key,
	})

	return d, tea.Batch(initCmd, sizeCmd, animationCmd)
}

func (d *manager) handleClose() (layout.Model, tea.Cmd) {
	return d.beginClose(false)
}

func (d *manager) beginClose(hiding bool) (layout.Model, tea.Cmd) {
	if len(d.stack) == 0 || d.stack[len(d.stack)-1].closing {
		return d, nil
	}
	d.drag.active = false
	top := len(d.stack) - 1
	d.stack[top].closeHovered = false
	if stopper, ok := d.stack[top].dialog.(interface{ StopAnimations() }); ok {
		stopper.StopAnimations()
	}
	resetDialogCloseHover(d.stack[top].dialog)
	cmd := d.stack[top].startClose(hiding)
	if !d.stack[top].anim.Running() {
		return d.handleTick(animation.TickMsg{})
	}
	return d, cmd
}

func (d *manager) handleHide() (layout.Model, tea.Cmd) {
	return d.beginClose(true)
}

// handleClosePlanDetail pops the top dialog only when it is a plan detail
// viewer showing exactly msg.Ref; anything else (another dialog on top, a
// detail for a different plan, an empty stack) is left untouched. Checking
// at apply time makes the close idempotent: duplicates for the same vanished
// plan cannot pop a second dialog.
func (d *manager) handleClosePlanDetail(msg ClosePlanDetailMsg) (layout.Model, tea.Cmd) {
	if len(d.stack) == 0 {
		return d, nil
	}
	if viewer, ok := d.stack[len(d.stack)-1].dialog.(PlanDetailViewer); ok && viewer.PlanRef() == msg.Ref {
		top := len(d.stack) - 1
		d.stack[top].cancel()
		CleanupDialog(d.stack[top].dialog)
		d.stack = d.stack[:top]
		d.drag.active = false
		return d, nil
	}
	return d, nil
}

// handleCloseAll closes all dialogs in the stack
func (d *manager) handleCloseAll() (layout.Model, tea.Cmd) {
	for i := range d.stack {
		d.stack[i].cancel()
		CleanupDialog(d.stack[i].dialog)
	}
	d.stack = nil
	d.drag.active = false
	return d, nil
}

// Cleanup immediately releases every dialog and animation owned by the
// manager. Integrators replacing a Manager must call Cleanup on the old
// instance first; unlike a fade-out, this reset is synchronous.
func (d *manager) Cleanup() {
	d.handleCloseAll()
}

// handleTick advances lifecycle transitions and removes completed closes.
func (d *manager) handleTick(msg animation.TickMsg) (layout.Model, tea.Cmd) {
	transitioning := false
	for i := range d.stack {
		if d.stack[i].opening() || d.stack[i].closing {
			transitioning = true
			break
		}
	}
	if transitioning {
		defer msg.MarkDirty()
	}
	remaining := d.stack[:0]
	var cmds []tea.Cmd
	for i := range d.stack {
		entry := &d.stack[i]
		finished, cmd := entry.tick("animation-tick", d.width, d.height)
		cmds = append(cmds, cmd)
		if finished {
			if !entry.hiding {
				CleanupDialog(entry.dialog)
			}
			continue
		}
		remaining = append(remaining, *entry)
	}
	clear(d.stack[len(remaining):])
	d.stack = remaining
	if d.Closing() || len(d.stack) == 0 {
		return d, tea.Batch(cmds...)
	}
	cmds = append(cmds, d.forwardToTopWithoutRetarget(msg))
	return d, tea.Batch(cmds...)
}

// Open returns true if there is at least one rendered dialog
func (d *manager) Open() bool {
	return len(d.stack) > 0
}

// HasActiveDialog reports whether a non-closing dialog remains.
func (d *manager) HasActiveDialog() bool {
	return len(d.stack) > 0 && !d.stack[len(d.stack)-1].closing
}

// Closing reports whether the visually topmost dialog is fading out.
func (d *manager) Closing() bool {
	return len(d.stack) > 0 && d.stack[len(d.stack)-1].closing
}

func (d *manager) pointerSuppressed() bool {
	if len(d.stack) == 0 {
		return false
	}
	top := d.stack[len(d.stack)-1]
	return top.opening() || top.closing
}

// TopIsExitConfirmation returns true if the topmost dialog is the exit
// confirmation dialog. Used by the top-level key handler to route ctrl+c to
// the exit confirmation (which exits the program) instead of stacking another
// exit confirmation on top.
func (d *manager) TopIsExitConfirmation() bool {
	if len(d.stack) == 0 {
		return false
	}
	_, ok := d.stack[len(d.stack)-1].dialog.(*exitConfirmationDialog)
	return ok
}

// TopIsBackground returns true if the topmost dialog is a background dialog
// (opened with a non-nil OriginatingEvent). See OpenDialogMsg for semantics.
func (d *manager) TopIsBackground() bool {
	if len(d.stack) == 0 || d.stack[len(d.stack)-1].closing {
		return false
	}
	return d.stack[len(d.stack)-1].originatingEvent != nil
}

// TopBackgroundEvent returns the originating event of the topmost background
// dialog, or nil if the top dialog is not a background dialog or the dialog
// stack is empty.
func (d *manager) TopBackgroundEvent() tea.Msg {
	if len(d.stack) == 0 || d.stack[len(d.stack)-1].closing {
		return nil
	}
	return d.stack[len(d.stack)-1].originatingEvent
}

// TopDialog returns the topmost dialog instance, or nil if the dialog stack
// is empty.
func (d *manager) TopDialog() Dialog {
	if !d.HasActiveDialog() {
		return nil
	}
	return d.stack[len(d.stack)-1].dialog
}

// HasDialog reports whether pred matches any dialog in the stack, bottom to
// top.
func (d *manager) HasDialog(pred func(Dialog) bool) bool {
	for i := range d.stack {
		if pred(d.stack[i].dialog) {
			return true
		}
	}
	return false
}

func (d *manager) takeTopVisualDirty() {
	if len(d.stack) == 0 {
		return
	}
	if dirty, ok := d.stack[len(d.stack)-1].dialog.(interface{ TakeVisualDirty() bool }); ok {
		changed := dirty.TakeVisualDirty()
		if changed {
			d.stack[len(d.stack)-1].invalidateView()
		}
		d.visualDirty = changed || d.visualDirty
	}
}

func (d *manager) TakeVisualDirty() bool {
	dirty := d.visualDirty
	d.visualDirty = false
	return dirty
}

func (d *manager) SetSize(width, height int) tea.Cmd {
	d.width = width
	d.height = height
	return nil
}

// CenterPosition calculates the centered position for a dialog given screen and dialog dimensions.
// Returns (row, col) suitable for use in Dialog.Position().
func CenterPosition(screenWidth, screenHeight, dialogWidth, dialogHeight int) (row, col int) {
	col = max(0, (screenWidth-dialogWidth)/2)
	row = max(0, (screenHeight-dialogHeight)/2)

	// Ensure dialog fits on screen
	col = min(col, max(0, screenWidth-dialogWidth))
	row = min(row, max(0, screenHeight-dialogHeight))

	return row, col
}

// GetLayers returns lipgloss layers for rendering all dialogs in the stack
// Dialogs are returned in order from bottom to top (index 0 is bottom-most)
func (d *manager) GetLayers() []*lipgloss.Layer {
	if len(d.stack) == 0 {
		return nil
	}

	layers := make([]*lipgloss.Layer, 0, len(d.stack))
	for i := range d.stack {
		e := &d.stack[i]
		view := d.entryView(e)
		row, col := e.position(d.width, d.height)
		layers = append(layers, lipgloss.NewLayer(view).X(col+e.offsetX).Y(row+e.offsetY))
	}
	return layers
}

func (d *manager) GetLayerInfos() []styles.LayerInfo {
	if len(d.stack) == 0 {
		return nil
	}
	layers := make([]styles.LayerInfo, 0, len(d.stack))
	for i := range d.stack {
		e := &d.stack[i]
		row, col := e.position(d.width, d.height)
		layers = append(layers, styles.LayerInfo{Content: d.entryView(e), X: col + e.offsetX, Y: row + e.offsetY})
	}
	return layers
}

func (d *manager) closeByModel(model Dialog) (layout.Model, tea.Cmd) {
	for i, entry := range slices.Backward(d.stack) {
		if entry.dialog != model {
			continue
		}
		if i == len(d.stack)-1 {
			return d.beginClose(false)
		}
		d.stack[i].cancel()
		CleanupDialog(d.stack[i].dialog)
		d.stack = slices.Delete(d.stack, i, i+1)
		d.visualDirty = true
		return d, nil
	}
	return d, nil
}
