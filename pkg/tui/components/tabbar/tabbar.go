// Package tabbar provides a horizontal tab bar for the TUI.
package tabbar

import (
	"maps"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// tabBarHeight is the number of terminal rows the tab bar occupies.
	tabBarHeight = 1
	// fallbackWidth is used until the caller allocates a width.
	fallbackWidth = 200
	// scrollArrowWidth is the visual width of a scroll indicator.
	scrollArrowWidth = 2
	// scrollLeftText is the left scroll arrow content.
	scrollLeftText = "◀ "
	// scrollRightText is the right scroll arrow content.
	scrollRightText = " ▶"
	// plusButtonWidth is the visual width of the "+" button.
	plusButtonWidth = 3
	// plusButtonText is the "+" button content.
	plusButtonText = " + "
	// noTab is the sentinel value for click zones that don't map to a tab.
	noTab = -1

	// dragHoldDelay is how long the mouse must be held down on a tab before
	// drag-and-drop mode activates. Prevents visual flash on normal clicks.
	dragHoldDelay = 200 * time.Millisecond

	// scrollStep is the number of columns scrolled per wheel tick or arrow click.
	scrollStep = 3

	scrollAnimDuration = 670 * time.Millisecond

	// scrollDelay is how long to wait after a tab change before starting
	// the scroll animation, giving the user time to orient.
	scrollDelay = 500 * time.Millisecond

	reorderAnimDuration = 330 * time.Millisecond

	dragReflowAnimDuration = 200 * time.Millisecond
	plusFeedbackDuration   = 280 * time.Millisecond
	plusHoverDuration      = 150 * time.Millisecond
)

// clickZone records where a clickable element is on the tab bar.
type clickZone struct {
	startX, endX  int
	tabIdx        int // index into tabs (noTab for non-tab zones)
	isPlus        bool
	isClose       bool
	isScrollLeft  bool
	isScrollRight bool
}

// DragHoldMsg is sent after dragHoldDelay to activate drag mode.
type DragHoldMsg struct {
	seq int // ties the timer to the click that started it
}

// ScrollDelayMsg is sent after scrollDelay to trigger the scroll-to-active animation.
type ScrollDelayMsg struct {
	seq int // ties the timer to the tab change that started it
}

// dragState tracks an in-progress tab drag-and-drop operation.
type dragState struct {
	pending    bool // mouse is down on a tab but hold timer hasn't fired yet
	active     bool // hold timer fired; full drag visuals shown
	dragIdx    int  // index of the tab being dragged
	dropIdx    int  // insertion index (-1 = no valid drop target)
	startX     int  // mouse X at the original click (preserved for grab-offset calculation)
	cursorX    int  // current mouse X while dragging
	grabOffset int  // distance from tab's left edge to the grab point (= startX - tab.start)
	seq        int  // monotonic counter to match hold timers to clicks
}

// isNoOp returns true when the drop would leave the tab in its original position.
func (d dragState) isNoOp() bool {
	return d.dropIdx == noTab || d.dropIdx == d.dragIdx || d.dropIdx == d.dragIdx+1
}

// tabBound records the screen-space left/right edges of a visible tab and its
// index in the full tab list, for drag-and-drop hit-testing.
type tabBound struct {
	start, end int
	tabIdx     int
	sessionID  string
}

// tabLayout holds pre-computed absolute column positions for each tab in the
// full (unclipped) tab strip.
type tabLayout struct {
	tab      Tab
	startCol int // absolute start column
	endCol   int // absolute end column (exclusive)
}

type settlingDropState struct {
	sessionID string
	currentX  float64 // interpolated local tabbar X at the last state transition
	targetX   float64 // authoritative clipped/rendered destination in tabbar coordinates
}

type layerInfo struct {
	Content string
	X, Y    int
}

func spacer(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

func composeLayers(width, height int, layers ...layerInfo) string {
	lines := make([]string, height)
	for i := range lines {
		lines[i] = spacer(width)
	}
	for _, layer := range layers {
		for row, content := range strings.Split(layer.Content, "\n") {
			y := layer.Y + row
			if y < 0 || y >= height || layer.X < 0 || layer.X >= width || content == "" {
				continue
			}
			w := ansi.StringWidth(content)
			if layer.X+w > width {
				content = ansi.Cut(content, 0, width-layer.X)
				w = ansi.StringWidth(content)
			}
			if w > 0 {
				lines[y] = ansi.Truncate(lines[y], layer.X, "") + content + ansi.TruncateLeft(lines[y], layer.X+w, "")
			}
		}
	}
	return strings.Join(lines, "\n")
}

// DragLayerInfo describes the floating tab layer for root composition.
type DragLayerInfo struct {
	Content string
	X, Y    int
}

// TabBar renders a horizontal bar of session tabs with click and keyboard support.
type TabBar struct {
	ar                *animation.Runtime
	tabs              []messages.TabInfo
	activeIdx         int
	width             int
	hidden            bool
	plusHovered       bool
	plusPressed       bool
	animationsStopped bool
	keyMap            KeyMap

	// scrollOffset is the number of columns of the full tab strip hidden
	// on the left. This enables smooth, sub-tab scrolling.
	scrollOffset int
	zones        []clickZone

	// dragBounds is the authoritative post-scroll, post-animation and post-clip
	// terminal-cell geometry used by rendering and every pointer phase.
	dragBounds []tabBound

	// maxTitleLen is the maximum display length for tab titles.
	// Configurable via user settings; defaults to the constant in tab.go.
	maxTitleLen int

	// lastEnsuredIdx tracks which active tab was last scrolled-to by
	// ensureActiveVisible. This prevents View() from overriding manual
	// scroll actions — ensureActiveVisible only runs when the active tab
	// actually changes.
	lastEnsuredIdx int

	// indicatorSub keeps the global animation tick chain alive while any tab
	// has an animated indicator (running, attention, loading). Without this
	// the tick chain can die when other transient animations (context bar,
	// editor shrink) complete, freezing the tab bar spinners.
	indicatorSub animation.Subscription

	// scrollAnim drives the ease-out scroll transition when the active tab
	// changes and the tab bar needs to scroll to reveal it.
	scrollAnim    animation.Transition
	scrollFrom    int  // scrollOffset at animation start
	scrollTo      int  // target scrollOffset
	scrollSeq     int  // monotonic counter to match scroll delay messages to tab changes
	scrollPending bool // true while waiting for the scroll delay to fire

	plusHoverAnim animation.Transition
	plusHoverFrom float64
	plusHoverTo   float64
	plusAnim      animation.Transition
	reorderAnim   animation.Transition
	reorderOffset map[string]int
	settleAnim    animation.Transition

	dragAnim       animation.Transition
	dragOffsetFrom map[string]int
	dragOffsetTo   map[string]int

	// settlingDrop tracks the brief post-drop overlay animation for the dragged
	// tab. During drag the tab is rendered as a floating overlay; after drop we
	// keep rendering that overlay and ease it into the tab's final slot while
	// the in-strip copy stays hidden, avoiding a visible snap or double render.
	settlingDrop *settlingDropState

	// lastDragSourceID is the session ID of the tab that was just dragged-and-
	// dropped. It is set in handleMouseRelease and consumed by
	// maybeStartReorderAnimation so the dropped tab's floating overlay can ease
	// into its final position while the displaced bystander tabs slide around it.
	lastDragSourceID string

	lastDropStartX int

	drag    dragState
	dragSeq int // monotonic counter incremented on each mouse-down

	// View cache: avoids re-rendering the tab bar every frame when nothing changed.
	cachedView           string
	cachedRender         renderedTabbar
	themeGeneration      uint64
	agentColorGeneration uint64
	viewDirty            bool
	visualDirty          bool
	lastOverlayX         int
	hadOverlay           bool

	visualGeneration uint64
}

// KeyMap defines key bindings for the tab bar.
type KeyMap struct {
	NewTab   key.Binding
	NextTab  key.Binding
	PrevTab  key.Binding
	CloseTab key.Binding
}

// DefaultKeyMap returns the default tab bar key bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		NewTab: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("Ctrl+t", "new tab"),
		),
		NextTab: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("Ctrl+n", "next tab"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("Ctrl+p", "prev tab"),
		),
		CloseTab: key.NewBinding(
			key.WithKeys("ctrl+w"),
			key.WithHelp("Ctrl+W", "close tab"),
		),
	}
}

// New creates a new tab bar with the given max title length.
// If maxTitleLen is <= 0, the default (20) is used.
func New(ar *animation.Runtime, maxTitleLen int) *TabBar {
	if ar == nil {
		panic("tabbar: nil animation runtime")
	}
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	t := &TabBar{
		ar:             ar,
		width:          fallbackWidth,
		indicatorSub:   ar.Subscribe(),
		scrollAnim:     ar.Transition(),
		reorderAnim:    ar.Transition(),
		settleAnim:     ar.Transition(),
		dragAnim:       ar.Transition(),
		plusAnim:       ar.Transition(),
		plusHoverAnim:  ar.Transition(),
		keyMap:         DefaultKeyMap(),
		maxTitleLen:    maxTitleLen,
		lastEnsuredIdx: noTab,
		drag:           dragState{dropIdx: noTab},
		viewDirty:      true,
	}
	t.installGeometry()
	return t
}

// SetWidth sets the available width for the tab bar.
func (t *TabBar) SetWidth(width int) tea.Cmd {
	width = max(0, width)
	defer t.recordVisualState()
	if width != t.width {
		t.visualGeneration++
		if t.drag.pending {
			t.drag = dragState{dropIdx: noTab}
			t.dragSeq++
		}
		t.captureSettlingPosition()
		t.width = width
		if width == 0 {
			t.StopAnimations()
		} else if !t.hidden {
			t.animationsStopped = false
		}
		t.zones = nil
		t.dragBounds = nil
		t.lastEnsuredIdx = noTab
		t.viewDirty = true
	}
	t.reconcileScroll()
	cmd := t.retargetSettlingDrop()
	if t.hasTabMotion() {
		return tea.Batch(cmd, t.ar.EnsureRunning())
	}
	return tea.Batch(cmd, t.syncIndicatorSub())
}

// SetCloseTabEnabled enables or disables the close-tab key binding.
func (t *TabBar) SetCloseTabEnabled(v bool) { t.keyMap.CloseTab.SetEnabled(v) }

// SetMaxTitleLength updates the title truncation limit.
func (t *TabBar) SetMaxTitleLength(n int) (cmd tea.Cmd) {
	defer t.recordVisualState()
	if n <= 0 {
		n = defaultMaxTitleLen
	}
	if n != t.maxTitleLen {
		t.visualGeneration++
		t.captureSettlingPosition()
		t.maxTitleLen, t.viewDirty = n, true
		t.lastEnsuredIdx = noTab
		t.zones = nil
		t.dragBounds = nil
		t.reconcileScroll()
		cmd = t.retargetSettlingDrop()
	}
	if t.hasTabMotion() {
		return tea.Batch(cmd, t.ar.EnsureRunning())
	}
	return tea.Batch(cmd, t.syncIndicatorSub())
}

// InvalidateCache is triggered when SetTabs updates the list of tabs and active index.
// to start a delayed scroll animation when the active tab changes.
func (t *TabBar) InvalidateCache() {
	t.viewDirty = true
	t.visualGeneration++
}

func (t *TabBar) SetTabs(tabs []messages.TabInfo, activeIdx int) tea.Cmd {
	t.visualGeneration++
	defer t.recordVisualState()
	if len(tabs) == 0 {
		activeIdx = noTab
	} else {
		activeIdx = max(0, min(activeIdx, len(tabs)-1))
	}
	t.viewDirty = true
	var cmds []tea.Cmd
	if activeIdx != t.activeIdx && !t.hidden && len(tabs) > 1 {
		if !t.scrollPending {
			t.armScrollPending()
		}
		t.scrollSeq++
		seq := t.scrollSeq
		cmds = append(cmds, tea.Tick(scrollDelay, func(time.Time) tea.Msg {
			return ScrollDelayMsg{seq: seq}
		}))
	}

	t.captureSettlingPosition()
	prevTabs := t.tabs
	prevLayouts := t.computeLayouts()
	// Index-based pointer gestures cannot survive a changed session ordering.
	if !sameTabOrder(prevTabs, tabs) {
		t.drag = dragState{dropIdx: noTab}
		t.dragSeq++
		t.zones = nil
		t.dragBounds = nil
	}
	t.tabs = append([]messages.TabInfo(nil), tabs...)
	t.activeIdx = activeIdx
	if t.Height() == 0 || t.width == 0 {
		t.StopAnimations()
		t.scrollOffset = 0
		return nil
	}
	t.animationsStopped = false
	t.clampScroll()
	t.reconcileScroll()
	if cmd := t.maybeStartReorderAnimation(prevTabs, prevLayouts); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, t.retargetSettlingDrop())
	if cmd := t.syncIndicatorSub(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if t.hasTabMotion() {
		cmds = append(cmds, t.ar.EnsureRunning())
	}
	return tea.Batch(cmds...)
}

func sameTabOrder(a, b []messages.TabInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SessionID != b[i].SessionID {
			return false
		}
	}
	return true
}

// SetVisible settles local motion and invalidates delayed gestures when hidden.
func (t *TabBar) SetVisible(visible bool) tea.Cmd {
	defer t.recordVisualState()
	if t.hidden == !visible {
		return nil
	}
	t.hidden = !visible
	t.visualGeneration++
	t.viewDirty = true
	if !visible {
		t.StopAnimations()
		return nil
	}
	t.animationsStopped = false
	t.lastEnsuredIdx = noTab
	t.reconcileScroll()
	return t.syncIndicatorSub()
}

// Tick advances local tab bar transitions and reports only visible frame changes.
func (t *TabBar) Tick() tea.Cmd {
	defer t.recordVisualState()
	if t.Height() == 0 {
		return nil
	}
	animated := t.plusHoverAnim.Running() || t.plusAnim.Running() || t.hasAnimatedIndicator() || t.scrollAnim.Running() || t.reorderAnim.Running() || t.settleAnim.Running() || t.dragAnim.Running() || t.settlingDrop != nil
	previousView, hadPreviousView := t.cachedView, t.cachedView != ""
	previousOverlayX, hadOverlay := t.lastOverlayX, t.hadOverlay
	if t.plusHoverAnim.Running() {
		t.plusHoverAnim.Tick()
	}
	if t.plusAnim.Running() {
		t.plusAnim.Tick()
		if !t.plusAnim.Running() {
			t.plusPressed = false
		}
	}
	if t.scrollAnim.Running() {
		t.scrollAnim.Tick()
		t.scrollOffset = t.scrollAnim.Lerp(t.scrollFrom, t.scrollTo)
	}
	if t.dragAnim.Running() {
		t.dragAnim.Tick()
		if !t.dragAnim.Running() && !t.IsDragging() {
			t.dragOffsetFrom = nil
			t.dragOffsetTo = nil
		}
	}
	if t.settleAnim.Running() {
		t.settleAnim.Tick()
		if !t.settleAnim.Running() {
			if t.settlingDrop != nil {
				t.settlingDrop.currentX = t.settlingDrop.targetX
			}
			t.endSettlingDrop()
		}
	}
	if t.reorderAnim.Running() {
		t.reorderAnim.Tick()
		if !t.reorderAnim.Running() {
			t.reorderOffset = nil
		}
	}
	t.reconcileScroll()
	cmd := t.retargetSettlingDrop()
	cmd = tea.Batch(cmd, t.syncIndicatorSub())
	if animated {
		t.viewDirty = true
		currentView := t.View()
		overlayChanged := hadOverlay != (t.settlingDrop != nil || t.drag.active)
		if t.settlingDrop != nil {
			overlayChanged = overlayChanged || !hadOverlay || t.settlingX() != previousOverlayX
		} else if t.drag.active {
			overlayChanged = overlayChanged || !hadOverlay || t.drag.cursorX-t.drag.grabOffset != previousOverlayX
		}
		changed := overlayChanged || !hadPreviousView || currentView != previousView
		t.visualDirty = t.visualDirty || changed
		if changed {
			t.visualGeneration++
		}
	}
	return cmd
}

// StopAnimations synchronously cancels every tab-owned transition and releases
// the runtime subscription. It is safe to call repeatedly during teardown.
func (t *TabBar) StopAnimations() {
	t.visualGeneration++
	defer t.recordVisualState()
	t.animationsStopped = true
	if t.scrollAnim.Running() {
		t.scrollOffset = t.scrollTo
	}
	t.scrollAnim.Cancel()
	t.reorderAnim.Cancel()
	t.settleAnim.Cancel()
	t.dragAnim.Cancel()
	t.plusAnim.Cancel()
	t.plusHoverAnim.Cancel()
	t.plusHoverFrom, t.plusHoverTo = 0, 0
	t.indicatorSub.Stop()
	t.scrollPending = false
	t.scrollSeq++
	t.dragSeq++
	t.zones = nil
	t.dragBounds = nil
	t.drag = dragState{dropIdx: noTab}
	t.plusHovered, t.plusPressed = false, false
	t.reorderOffset = nil
	t.dragOffsetFrom = nil
	t.dragOffsetTo = nil
	t.settlingDrop = nil
	t.lastDragSourceID = ""
	t.viewDirty = true
}

// Height returns the height of the tab bar.
func (t *TabBar) Height() int {
	if t.hidden {
		return 0
	}
	return tabBarHeight
}

// IsAnimating reports finite tab motion or new-tab click feedback.
func (t *TabBar) IsAnimating() bool {
	return t.plusHoverAnim.Running() || t.plusAnim.Running() || t.hasTabMotion()
}

func (t *TabBar) hasTabMotion() bool {
	return t.scrollAnim.Running() || t.reorderAnim.Running() || t.settleAnim.Running() || t.dragAnim.Running()
}

// HasPointerCapture keeps releases routed here after the pointer leaves the strip.
func (t *TabBar) HasPointerCapture() bool { return t.IsDragging() || t.plusPressed }

// IsDragging returns true when a tab drag is in progress or pending.
func (t *TabBar) IsDragging() bool {
	return t.drag.active || t.drag.pending
}

// HasFloatingOverlay returns true when the tab bar needs to render a
// floating overlay layer — either because a drag is active/pending or
// because a post-drop settling animation is in progress.
func (t *TabBar) HasFloatingOverlay() bool {
	return t.drag.active || t.drag.pending || t.settlingDrop != nil
}

// VisualGeneration changes only when rendered tab geometry or drag styling changes.
func (t *TabBar) VisualGeneration() uint64 { return t.visualGeneration + styles.AgentColorGeneration() }

// Bindings returns distinct actions, never combining unrelated tab operations.
func (t *TabBar) Bindings() []key.Binding {
	return []key.Binding{t.keyMap.NewTab, t.keyMap.CloseTab, t.keyMap.PrevTab, t.keyMap.NextTab}
}

// Count reports the number of rendered session tabs.
func (t *TabBar) Count() int { return len(t.tabs) }

// Update handles messages and returns commands.
func (t *TabBar) Update(msg tea.Msg) (cmd tea.Cmd) {
	if motion, ok := msg.(tea.MouseMotionMsg); ok {
		if t.Height() == 0 {
			return nil
		}
		t.installGeometry()
		hovered := t.plusAt(motion.X, motion.Y)
		if hovered == t.plusHovered && (!t.IsDragging() || motion.X == t.drag.cursorX) {
			return nil
		}
	}
	defer t.recordVisualState()
	defer func() {
		if _, keypress := msg.(tea.KeyPressMsg); keypress {
			return
		}
		t.visualGeneration++
		t.reconcileScroll()
		if t.hasTabMotion() {
			cmd = tea.Batch(cmd, t.ar.EnsureRunning())
		} else {
			cmd = tea.Batch(cmd, t.syncIndicatorSub())
		}
	}()
	t.viewDirty = true
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, t.keyMap.NewTab):
			return core.CmdHandler(messages.SpawnSessionMsg{})

		case key.Matches(msg, t.keyMap.NextTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			nextIdx := (t.activeIdx + 1) % len(t.tabs)
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[nextIdx].SessionID})

		case key.Matches(msg, t.keyMap.PrevTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			prevIdx := t.activeIdx - 1
			if prevIdx < 0 {
				prevIdx = len(t.tabs) - 1
			}
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[prevIdx].SessionID})

		case key.Matches(msg, t.keyMap.CloseTab):
			if len(t.tabs) <= 1 {
				return nil
			}
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[t.activeIdx].SessionID})
		}

	case DragHoldMsg:
		if t.Height() == 0 || t.drag.dragIdx < 0 || t.drag.dragIdx >= len(t.tabs) || !t.drag.pending || msg.seq != t.drag.seq {
			return nil
		}
		t.visualDirty = true
		t.drag.pending = false
		t.drag.active = true
		// Compute the grab offset: how far into the tab the user clicked.
		// dragBounds was populated by the most recent View() call, so the
		// drag source's screen position is available here.
		for _, b := range t.dragBounds {
			if b.tabIdx == t.drag.dragIdx {
				t.drag.grabOffset = t.drag.startX - b.start
				break
			}
		}
		if t.drag.grabOffset < 0 {
			t.drag.grabOffset = 0
		}
		if t.settlingDrop != nil && t.drag.dragIdx >= 0 && t.drag.dragIdx < len(t.tabs) && t.settlingDrop.sessionID == t.tabs[t.drag.dragIdx].SessionID {
			t.settleAnim.Cancel()
			t.settlingDrop = nil
		}
		cmds := []tea.Cmd{t.updateDragReflow()}
		if cmd := t.syncIndicatorSub(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return tea.Batch(cmds...)

	case ScrollDelayMsg:
		if t.Height() == 0 || !t.scrollPending || msg.seq != t.scrollSeq {
			return nil
		}
		t.scrollPending = false
		return t.startScrollAnimation()

	case tea.MouseClickMsg:
		if t.Height() == 0 || msg.Y != 0 {
			return nil
		}
		t.installGeometry()
		if msg.Button == tea.MouseLeft {
			hoverCmd := t.setPlusHovered(t.plusAt(msg.X, msg.Y))
			t.plusPressed = t.plusHovered
			t.viewDirty = true
			if t.plusHovered {
				t.visualDirty = true
				return tea.Batch(hoverCmd, t.plusAnim.Start(plusFeedbackDuration, animation.EaseOutCubic), t.handleLeftClickDown(msg.X))
			}
			return tea.Batch(hoverCmd, t.handleLeftClickDown(msg.X))
		}
		if t.IsDragging() {
			return t.handleMouseRelease(t.drag.cursorX)
		}
		if msg.Button == tea.MouseMiddle {
			return t.handleMiddleClick(msg.X)
		}
		return nil

	case tea.BlurMsg:
		t.plusHoverAnim.Cancel()
		t.plusHoverFrom, t.plusHoverTo = 0, 0
		t.plusAnim.Cancel()
		t.plusHovered, t.plusPressed = false, false
		if t.drag.active {
			return t.handleMouseRelease(t.drag.cursorX)
		}
		if t.drag.pending {
			t.drag = dragState{dropIdx: noTab}
		}
		return nil

	case messages.WheelCoalescedMsg:
		if t.Height() == 0 {
			return nil
		}
		t.captureSettlingPosition()
		t.scrollAnim.Cancel()
		t.scrollPending = false
		t.scrollSeq++
		t.lastEnsuredIdx = t.activeIdx
		t.scrollOffset = max(0, t.scrollOffset+msg.Delta*scrollStep)
		t.reconcileScroll()
		return t.retargetSettlingDrop()

	case tea.MouseMotionMsg:
		if t.Height() == 0 {
			return nil
		}
		t.installGeometry()
		hoverCmd := t.setPlusHovered(t.plusAt(msg.X, msg.Y))
		t.viewDirty = true
		return tea.Batch(hoverCmd, t.handleMouseMotion(msg.X))

	case tea.MouseReleaseMsg:
		t.plusPressed = false
		hoverCmd := t.setPlusHovered(t.plusAt(msg.X, msg.Y))
		return tea.Batch(hoverCmd, t.handleMouseRelease(msg.X))
	}

	return nil
}

func (t *TabBar) hoverFraction() float64 {
	if t.plusHoverAnim.Running() {
		return t.plusHoverFrom + (t.plusHoverTo-t.plusHoverFrom)*t.plusHoverAnim.Value()
	}
	return t.plusHoverTo
}

func (t *TabBar) setPlusHovered(hovered bool) tea.Cmd {
	if t.plusHovered == hovered {
		return nil
	}
	t.plusHoverFrom = t.hoverFraction()
	t.plusHovered = hovered
	t.plusHoverTo = 0
	if hovered {
		t.plusHoverTo = 1
	}
	t.viewDirty = true
	if t.plusHoverFrom == t.plusHoverTo {
		t.plusHoverAnim.Cancel()
		return nil
	}
	return t.plusHoverAnim.Start(plusHoverDuration, animation.EaseOutCubic)
}

func (t *TabBar) plusAt(x, y int) bool {
	if y != 0 || t.Height() == 0 {
		return false
	}
	for _, z := range t.zones {
		if z.isPlus && x >= z.startX && x < z.endX {
			return true
		}
	}
	return false
}

func (t *TabBar) TakeVisualDirty() bool {
	dirty := t.visualDirty
	t.visualDirty = false
	if dirty {
		t.visualGeneration++
	}
	return dirty
}

// handleLeftClickDown initiates a drag or handles a normal click.
func (t *TabBar) handleLeftClickDown(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}
		if z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && !z.isClose {
			t.dragSeq++
			seq := t.dragSeq
			t.drag = dragState{pending: true, dragIdx: z.tabIdx, dropIdx: noTab, startX: x, cursorX: x, seq: seq}
			return tea.Tick(dragHoldDelay, func(time.Time) tea.Msg { return DragHoldMsg{seq: seq} })
		}
		break
	}
	return t.handleClick(x)
}

// handleMouseMotion updates the drag cursor and animates bystander tabs out of
// the way in real time once the dragged tab edges cross neighbor midpoints.
func (t *TabBar) handleMouseMotion(x int) tea.Cmd {
	if !t.drag.pending && !t.drag.active {
		return nil
	}

	previousX, previousDrop := t.drag.cursorX, t.drag.dropIdx
	t.drag.cursorX = x
	if t.drag.pending {
		// Do NOT update startX: it records the original click position and
		// is used to compute grabOffset when DragHoldMsg fires.
		return nil
	}

	cmd := t.updateDragReflow()
	t.visualDirty = t.visualDirty || previousX != t.drag.cursorX || previousDrop != t.drag.dropIdx
	return cmd
}

// handleMouseRelease completes a drag or falls back to a click.
func (t *TabBar) handleMouseRelease(x int) (cmd tea.Cmd) {
	defer t.recordVisualState()
	defer func() {
		if t.hasTabMotion() {
			cmd = tea.Batch(cmd, t.ar.EnsureRunning())
		}
	}()
	if !t.drag.active && !t.drag.pending {
		return nil
	}

	t.visualDirty = true
	if t.drag.pending {
		t.drag = dragState{dropIdx: noTab}
		t.dragAnim.Cancel()
		t.dragOffsetFrom = nil
		t.dragOffsetTo = nil
		return t.handleClick(x)
	}

	t.drag.cursorX = x
	from := t.drag.dragIdx
	to := t.drag.dropIdx
	noop := t.drag.isNoOp()
	// Every active release keeps the exact floating X and hands it to an
	// independent settle transition. This also covers drops that do not reorder.
	if from >= 0 && from < len(t.tabs) {
		sessionID := t.tabs[from].SessionID
		startX := t.clampFloatingX(t.drag.cursorX-t.drag.grabOffset, t.computeLayouts()[from].tab.Width())
		targetX := startX
		layouts := t.computeLayouts()
		viewStart, _, cursor := t.currentTabViewMetrics()
		if from < len(layouts) {
			targetX = cursor + layouts[from].startCol - viewStart
		}
		if !noop {
			t.lastDragSourceID = sessionID
			t.lastDropStartX = startX
			// The preview layout already exposes the destination occupied by the
			// dragged tab after insertion.
			preview := t.previewTabs()
			for i, tab := range preview {
				if tab.SessionID == sessionID {
					layouts := t.computeLayoutsForTabs(preview)
					viewStart, _, cursor := t.currentTabViewMetrics()
					targetX = cursor + layouts[i].startCol - viewStart
					break
				}
			}
		}
		cmd = t.beginSettlingDrop(sessionID, float64(startX), float64(targetX))
	}
	t.drag = dragState{dropIdx: noTab}
	t.dragAnim.Cancel()

	if noop {
		t.dragOffsetFrom = nil
		t.dragOffsetTo = nil
		return tea.Batch(cmd, t.syncIndicatorSub())
	}

	// Keep bystander offsets alive until SetTabs arrives.
	t.dragOffsetFrom = nil

	finalTo := to
	if to > from {
		finalTo--
	}

	return tea.Batch(cmd, core.CmdHandler(messages.ReorderTabMsg{FromIdx: from, ToIdx: finalTo}))
}

func (t *TabBar) previewTabs() []messages.TabInfo {
	preview := append([]messages.TabInfo(nil), t.tabs...)
	if !t.drag.active || t.drag.dragIdx < 0 || t.drag.dragIdx >= len(preview) || t.drag.dropIdx == noTab {
		return preview
	}
	tab := preview[t.drag.dragIdx]
	preview = append(preview[:t.drag.dragIdx], preview[t.drag.dragIdx+1:]...)
	idx := t.drag.dropIdx
	if idx > t.drag.dragIdx {
		idx--
	}
	idx = max(0, min(idx, len(preview)))
	preview = append(preview, messages.TabInfo{})
	copy(preview[idx+1:], preview[idx:])
	preview[idx] = tab
	return preview
}

func (t *TabBar) currentTabViewMetrics() (viewStart, viewWidth, cursor int) {
	if t.width == 0 {
		return 0, 0, 0
	}
	fullWidth := t.width
	selectorW := 0
	rightControlsWidth := plusButtonWidth + selectorW
	layouts := t.computeLayouts()
	totalTabWidth := 0
	if len(layouts) > 0 {
		totalTabWidth = layouts[len(layouts)-1].endCol
	}
	availWidth := fullWidth - rightControlsWidth
	needsScroll := totalTabWidth > availWidth
	if needsScroll {
		rightControlsWidth += scrollArrowWidth
		availWidth = fullWidth - rightControlsWidth
	}
	maxScroll := max(totalTabWidth-availWidth, 0)
	scrollOffset := max(0, min(t.scrollOffset, maxScroll))
	viewStart = scrollOffset
	viewWidth = max(0, availWidth)
	cursor = 0
	if !needsScroll {
		viewWidth = totalTabWidth
	}
	if needsScroll && scrollOffset > 0 {
		viewStart += scrollArrowWidth
		viewWidth = max(0, viewWidth-scrollArrowWidth)
		cursor = scrollArrowWidth
	}
	return viewStart, viewWidth, cursor
}

func (t *TabBar) localXToStripX(localX int) int {
	viewStart, _, cursor := t.currentTabViewMetrics()
	return viewStart + (localX - cursor)
}

func clipLayerSegment(content string, x, width int) (string, int, bool) {
	if content == "" || width <= 0 {
		return "", 0, false
	}
	contentWidth := ansi.StringWidth(content)
	if contentWidth <= 0 {
		return "", 0, false
	}
	start := 0
	if x < 0 {
		start = -x
		x = 0
	}
	if x >= width || start >= contentWidth {
		return "", 0, false
	}
	end := min(contentWidth, start+(width-x))
	if end <= start {
		return "", 0, false
	}
	return ansi.Cut(content, start, end), x, true
}

func (t *TabBar) dragPreviewOffset(sessionID string) int {
	if len(t.dragOffsetTo) == 0 {
		return 0
	}
	from := t.dragOffsetFrom[sessionID]
	to := t.dragOffsetTo[sessionID]
	if !t.dragAnim.Running() {
		return to
	}
	return t.dragAnim.Lerp(from, to)
}

func (t *TabBar) setDragPreviewOffsets(target map[string]int) tea.Cmd {
	from := make(map[string]int, len(target)+len(t.dragOffsetTo))
	toAll := make(map[string]int, len(target)+len(t.dragOffsetTo))
	for sessionID := range t.dragOffsetTo {
		toAll[sessionID] = 0
	}
	maps.Copy(toAll, target)

	changed := false
	for sessionID, to := range toAll {
		cur := t.dragPreviewOffset(sessionID)
		from[sessionID] = cur
		if cur != to {
			changed = true
		}
	}
	if !changed {
		t.dragAnim.Cancel()
		t.dragOffsetFrom = nil
		t.dragOffsetTo = toAll
		return nil
	}

	t.dragOffsetFrom = from
	t.dragOffsetTo = toAll
	return t.dragAnim.Start(dragReflowAnimDuration, animation.EaseOutQuint)
}

func (t *TabBar) updateDragReflow() tea.Cmd {
	if !t.drag.active || t.drag.dragIdx < 0 || t.drag.dragIdx >= len(t.tabs) {
		return nil
	}
	layouts := t.computeLayouts()
	if len(layouts) == 0 || t.drag.dragIdx >= len(layouts) {
		return nil
	}

	dragSessionID := t.tabs[t.drag.dragIdx].SessionID
	dragLay := layouts[t.drag.dragIdx]
	dragWidth := dragLay.endCol - dragLay.startCol
	dragLeft := t.localXToStripX(t.drag.cursorX - t.drag.grabOffset)
	dragRight := dragLeft + dragWidth

	previewTabs := append([]messages.TabInfo(nil), t.tabs...)
	previewIdx := t.drag.dragIdx
	for {
		moved := false
		if previewIdx+1 < len(previewTabs) {
			rightNeighborLay := layouts[previewIdx+1]
			rightMid := (rightNeighborLay.startCol + rightNeighborLay.endCol) / 2
			if dragRight > rightMid {
				previewTabs[previewIdx], previewTabs[previewIdx+1] = previewTabs[previewIdx+1], previewTabs[previewIdx]
				layouts = t.computeLayoutsForTabs(previewTabs)
				previewIdx++
				moved = true
			}
		}
		if !moved && previewIdx > 0 {
			leftNeighborLay := layouts[previewIdx-1]
			leftMid := (leftNeighborLay.startCol + leftNeighborLay.endCol) / 2
			if dragLeft < leftMid {
				previewTabs[previewIdx], previewTabs[previewIdx-1] = previewTabs[previewIdx-1], previewTabs[previewIdx]
				layouts = t.computeLayoutsForTabs(previewTabs)
				previewIdx--
				moved = true
			}
		}
		if !moved {
			break
		}
	}

	dropIdx := previewIdx
	if previewIdx > t.drag.dragIdx {
		dropIdx = previewIdx + 1
	}
	t.drag.dropIdx = dropIdx

	baseLayouts := t.computeLayouts()
	previewLayouts := t.computeLayoutsForTabs(previewTabs)
	baseByID := make(map[string]int, len(baseLayouts))
	previewByID := make(map[string]int, len(previewLayouts))
	for i, tab := range t.tabs {
		if i < len(baseLayouts) {
			baseByID[tab.SessionID] = baseLayouts[i].startCol
		}
	}
	for i, tab := range previewTabs {
		if i < len(previewLayouts) {
			previewByID[tab.SessionID] = previewLayouts[i].startCol
		}
	}

	target := make(map[string]int)
	for _, tab := range t.tabs {
		if tab.SessionID == dragSessionID {
			continue
		}
		baseX, okBase := baseByID[tab.SessionID]
		previewX, okPreview := previewByID[tab.SessionID]
		if !okBase || !okPreview {
			continue
		}
		if delta := previewX - baseX; delta != 0 {
			target[tab.SessionID] = delta
		}
	}

	cmd := t.setDragPreviewOffsets(target)

	return cmd
}

// handleMiddleClick closes the tab under the cursor on middle-click.
func (t *TabBar) handleMiddleClick(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}
		if z.tabIdx >= 0 && z.tabIdx < len(t.tabs) {
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}
	return nil
}

// handleClick uses the click zones computed during the last View() call.
func (t *TabBar) handleClick(x int) tea.Cmd {
	for _, z := range t.zones {
		if x < z.startX || x >= z.endX {
			continue
		}

		switch {
		case z.isScrollLeft:
			t.scrollAnim.Cancel()
			t.scrollPending = false
			t.scrollOffset = max(0, t.scrollOffset-scrollStep)
			return nil
		case z.isScrollRight:
			t.scrollAnim.Cancel()
			t.scrollPending = false
			t.scrollOffset += scrollStep
			t.clampScroll()
			return nil
		case z.isPlus:
			return core.CmdHandler(messages.SpawnSessionMsg{})
		case z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		case z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && z.tabIdx != t.activeIdx:
			t.armScrollPending()
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}

	return nil
}

type renderedTabbar struct {
	view       string
	zones      []clickZone
	dragBounds []tabBound
}

// View reads rendered output; pointer geometry is installed by state updates.
func (t *TabBar) View() string { return t.rendered().view }

func (t *TabBar) rendered() renderedTabbar {
	if !t.viewDirty && !t.drag.active && t.cachedView != "" && t.themeGeneration == styles.ThemeGeneration() && t.agentColorGeneration == styles.AgentColorGeneration() {
		return t.cachedRender
	}
	t.cachedRender = t.render()
	t.cachedView = t.cachedRender.view
	t.viewDirty = false
	t.themeGeneration = styles.ThemeGeneration()
	t.agentColorGeneration = styles.AgentColorGeneration()
	return t.cachedRender
}

func (t *TabBar) installGeometry() {
	result := t.rendered()
	t.zones = result.zones
	t.dragBounds = result.dragBounds
}

func (t *TabBar) render() renderedTabbar {
	var result renderedTabbar
	if t.Height() == 0 || t.width == 0 {
		return result
	}
	fullWidth := t.width

	selectorW := 0

	rightControlsWidth := plusButtonWidth + selectorW
	layouts := t.computeLayouts()
	totalTabWidth := 0
	if len(layouts) > 0 {
		totalTabWidth = layouts[len(layouts)-1].endCol
	}

	availWidth := fullWidth - rightControlsWidth
	needsScroll := totalTabWidth > availWidth
	if needsScroll {
		rightControlsWidth += scrollArrowWidth
		availWidth = fullWidth - rightControlsWidth
	}

	maxScroll := max(totalTabWidth-availWidth, 0)
	scrollOffset := max(0, min(t.scrollOffset, maxScroll))

	showLeftArrow := needsScroll && scrollOffset > 0
	showRightArrow := needsScroll && scrollOffset < maxScroll && fullWidth >= plusButtonWidth+scrollArrowWidth

	tabViewStart := scrollOffset
	tabViewWidth := max(0, availWidth)
	if !needsScroll {
		tabViewWidth = totalTabWidth
	}
	if showLeftArrow {
		tabViewStart += scrollArrowWidth
		tabViewWidth = max(0, tabViewWidth-scrollArrowWidth)
	}

	chromeFg := styles.MutedContrastFg(styles.Background)
	chromeBg := lipgloss.NewStyle().Background(styles.Background)
	plusStyle := chromeBg.Foreground(chromeFg)
	plusBg, plusFg := styles.Background, chromeFg
	hoverStrength := 0.3 * t.hoverFraction()
	plusBg = blendColors(plusBg, styles.TabHoverBg, hoverStrength)
	plusFg = blendColors(plusFg, styles.TabHoverFg, hoverStrength)
	if t.plusAnim.Running() {
		strength := 0.7 * (1 - t.plusAnim.Value())
		plusBg = blendColors(plusBg, styles.TabDragBg, strength)
		plusFg = blendColors(plusFg, styles.TabDragFg, strength)
	}
	plusStyle = plusStyle.Background(plusBg).Foreground(plusFg)
	arrowStyle := chromeBg.Foreground(chromeFg)
	attnArrowStyle := chromeBg.Foreground(styles.EnsureContrast(styles.Warning, styles.Background)).Bold(true)

	var line string
	var cursor int

	if showLeftArrow {
		style := arrowStyle
		if t.hasAttentionBefore(layouts, tabViewStart) {
			style = attnArrowStyle
		}
		line += style.Render(scrollLeftText)
		result.zones = append(result.zones, clickZone{startX: cursor, endX: cursor + scrollArrowWidth, tabIdx: noTab, isScrollLeft: true})
		cursor += scrollArrowWidth
	}

	tabLayers := []layerInfo{{Content: spacer(tabViewWidth), X: 0, Y: 0}}
	for i, lay := range layouts {
		offsetX := t.dragPreviewOffset(t.tabs[i].SessionID) + t.reorderVisualOffset(t.tabs[i].SessionID)
		renderX := lay.startCol - tabViewStart + offsetX
		seg, finalX, visible := clipLayerSegment(lay.tab.View(), renderX, tabViewWidth)
		if !visible {
			continue
		}
		visWidth := ansi.StringWidth(seg)
		screenX := cursor + finalX

		switch {
		case t.drag.active && i == t.drag.dragIdx:
			// Leave the dragged tab's original slot blank. Do NOT render an
			// explicit spacer here: bystanders that shifted into this slot via
			// dragPreviewOffset are appended to tabLayers before this iteration
			// completes, so a spacer added afterwards would paint over them and
			// make them disappear. The background spacer (first layer) already
			// covers every unclaimed column in the tab area.
			continue
		case t.settlingDrop != nil && t.tabs[i].SessionID == t.settlingDrop.sessionID:
			// The overlay exclusively owns rendering, but its clipped terminal-cell
			// geometry remains authoritative for immediate re-grab.
			overlayX := t.settlingX()
			clipStart := max(cursor, overlayX)
			clipEnd := min(cursor+tabViewWidth, overlayX+lay.tab.Width())
			if clipEnd > clipStart {
				result.dragBounds = append(result.dragBounds, tabBound{start: clipStart, end: clipEnd, tabIdx: i, sessionID: t.tabs[i].SessionID})
				mainEnd := min(clipEnd, overlayX+lay.tab.MainZoneEnd())
				if mainEnd > clipStart {
					result.zones = append(result.zones, clickZone{startX: clipStart, endX: mainEnd, tabIdx: i})
				}
				closeStart := max(clipStart, overlayX+lay.tab.MainZoneEnd())
				if clipEnd > closeStart {
					result.zones = append(result.zones, clickZone{startX: closeStart, endX: clipEnd, tabIdx: i, isClose: true})
				}
			}
			continue
		default:
			result.dragBounds = append(result.dragBounds, tabBound{start: screenX, end: screenX + visWidth, tabIdx: i, sessionID: t.tabs[i].SessionID})
			tabLayers = append(tabLayers, layerInfo{Content: seg, X: finalX, Y: 0})
		}

		mainStart := max(0, renderX)
		mainEnd := min(tabViewWidth, renderX+lay.tab.MainZoneEnd())
		if mainEnd > mainStart {
			result.zones = append(result.zones, clickZone{startX: cursor + mainStart, endX: cursor + mainEnd, tabIdx: i})
		}
		closeStart := max(0, renderX+lay.tab.MainZoneEnd())
		closeEnd := min(tabViewWidth, renderX+lay.tab.Width())
		if closeEnd > closeStart {
			result.zones = append(result.zones, clickZone{startX: cursor + closeStart, endX: cursor + closeEnd, tabIdx: i, isClose: true})
		}
	}

	line += composeLayers(tabViewWidth, 1, tabLayers...)
	cursor += tabViewWidth

	rightArrowSpace := 0
	if showRightArrow {
		rightArrowSpace = scrollArrowWidth
	}
	firstRightPos := cursor
	if needsScroll {
		firstRightPos = fullWidth - selectorW - plusButtonWidth - rightArrowSpace
	}
	gap := firstRightPos - cursor
	if gap > 0 {
		line += spacer(gap)
		cursor += gap
	}

	if showRightArrow {
		style := arrowStyle
		if t.hasAttentionAfter(layouts, tabViewStart+tabViewWidth) {
			style = attnArrowStyle
		}
		line += style.Render(scrollRightText)
		result.zones = append(result.zones, clickZone{startX: cursor, endX: cursor + scrollArrowWidth, tabIdx: noTab, isScrollRight: true})
		cursor += scrollArrowWidth
	}

	line += plusStyle.Render(plusButtonText)
	result.zones = append(result.zones, clickZone{startX: cursor, endX: cursor + plusButtonWidth, tabIdx: noTab, isPlus: true})
	cursor += plusButtonWidth
	line += spacer(max(0, fullWidth-cursor))

	if fullWidth < plusButtonWidth {
		line = plusStyle.Render("+" + spacer(fullWidth-1))
		result.zones = []clickZone{{startX: 0, endX: fullWidth, tabIdx: noTab, isPlus: true}}
		result.dragBounds = nil
	}
	line = ansi.Truncate(line, fullWidth, "")
	for i := range result.zones {
		result.zones[i].startX = max(0, min(result.zones[i].startX, fullWidth))
		result.zones[i].endX = max(result.zones[i].startX, min(result.zones[i].endX, fullWidth))
	}
	result.view = line
	return result
}

func (t *TabBar) ensureActiveVisible(layouts []tabLayout, availWidth int) {
	if t.activeIdx < 0 || t.activeIdx >= len(layouts) {
		return
	}
	lay := layouts[t.activeIdx]
	rightEdge := t.scrollOffset + availWidth
	if lay.endCol > rightEdge {
		t.scrollOffset = lay.endCol - availWidth
		if t.scrollOffset > 0 {
			t.scrollOffset = lay.endCol - availWidth + scrollArrowWidth
		}
	}

	leftEdge := t.scrollOffset
	if t.scrollOffset > 0 {
		leftEdge += scrollArrowWidth
	}
	if lay.startCol < leftEdge {
		t.scrollOffset = lay.startCol
		if t.scrollOffset > 0 {
			t.scrollOffset = max(0, lay.startCol-scrollArrowWidth)
		}
	}
}

func (t *TabBar) hasAttentionBefore(layouts []tabLayout, col int) bool {
	for i, lay := range layouts {
		if lay.startCol >= col {
			break
		}
		if t.tabs[i].NeedsAttention {
			return true
		}
	}
	return false
}

func (t *TabBar) hasAttentionAfter(layouts []tabLayout, col int) bool {
	for i, lay := range layouts {
		if lay.endCol <= col {
			continue
		}
		if lay.startCol >= col && t.tabs[i].NeedsAttention {
			return true
		}
	}
	return false
}

func (t *TabBar) computeLayouts() []tabLayout {
	return t.computeLayoutsForTabs(t.tabs)
}

func (t *TabBar) computeLayoutsForTabs(tabs []messages.TabInfo) []tabLayout {
	layouts := make([]tabLayout, len(tabs))
	totalWidth := 0
	dragSessionID := ""
	if t.drag.active && t.drag.dragIdx >= 0 && t.drag.dragIdx < len(t.tabs) {
		dragSessionID = t.tabs[t.drag.dragIdx].SessionID
	}
	for i, info := range tabs {
		role := dragRoleNone
		if dragSessionID != "" && info.SessionID == dragSessionID {
			role = dragRoleSource
		}
		tab := renderTab(info, t.maxTitleLen, role, t.ar.Now())
		layouts[i] = tabLayout{tab: tab, startCol: totalWidth, endCol: totalWidth + tab.Width()}
		totalWidth += tab.Width()
	}
	return layouts
}

func (t *TabBar) armScrollPending() {
	t.scrollAnim.Cancel()
	t.scrollFrom = t.scrollOffset
	t.scrollPending = true
	t.lastEnsuredIdx = noTab
}

func (t *TabBar) startScrollAnimation() tea.Cmd {
	if len(t.tabs) <= 1 {
		return nil
	}

	layouts := t.computeLayouts()
	totalTabWidth := 0
	if len(layouts) > 0 {
		totalTabWidth = layouts[len(layouts)-1].endCol
	}

	fullWidth := t.width
	selectorW := 0
	availWidth := fullWidth - plusButtonWidth - selectorW
	needsScroll := totalTabWidth > availWidth
	if !needsScroll {
		t.lastEnsuredIdx = t.activeIdx
		return nil
	}
	availWidth -= scrollArrowWidth

	from := t.scrollOffset
	t.ensureActiveVisible(layouts, availWidth)
	to := t.scrollOffset
	t.scrollOffset = from

	if from == to {
		t.lastEnsuredIdx = t.activeIdx
		return nil
	}

	t.scrollFrom = from
	t.scrollTo = to
	t.lastEnsuredIdx = t.activeIdx
	return t.scrollAnim.Start(scrollAnimDuration, animation.EaseOutQuint)
}

func (t *TabBar) clampScroll() { t.scrollOffset = max(0, t.scrollOffset) }

func (t *TabBar) reconcileScroll() {
	if t.width == 0 || len(t.tabs) <= 1 {
		t.scrollOffset = 0
		t.scrollAnim.Cancel()
		t.scrollPending = false
		return
	}

	layouts := t.computeLayouts()
	totalTabWidth := 0
	if len(layouts) > 0 {
		totalTabWidth = layouts[len(layouts)-1].endCol
	}

	fullWidth := t.width
	selectorW := 0
	availWidth := fullWidth - plusButtonWidth - selectorW
	needsScroll := totalTabWidth > availWidth
	if needsScroll {
		availWidth -= scrollArrowWidth
	}

	if !needsScroll {
		t.scrollOffset = 0
		t.scrollAnim.Cancel()
		t.scrollPending = false
	} else if t.activeIdx != t.lastEnsuredIdx && !t.scrollAnim.Running() && !t.scrollPending {
		t.ensureActiveVisible(layouts, availWidth)
		t.lastEnsuredIdx = t.activeIdx
	}
	t.scrollOffset = max(0, min(t.scrollOffset, max(0, totalTabWidth-availWidth)))
}

func (t *TabBar) hasAnimatedIndicator() bool {
	if t.Height() == 0 || t.width == 0 || t.animationsStopped {
		return false
	}
	viewStart, viewWidth, _ := t.currentTabViewMetrics()
	for i, layout := range t.computeLayouts() {
		tab := t.tabs[i]
		if tab.NeedsAttention || (tab.Activity != messages.TabActivityRunning && (tab.Activity != messages.TabActivityNone || !tab.IsRunning)) {
			continue
		}
		if layout.endCol > viewStart && layout.startCol < viewStart+viewWidth {
			return true
		}
		if t.drag.active && t.drag.dragIdx == i || t.settlingDrop != nil && t.settlingDrop.sessionID == tab.SessionID {
			return true
		}
	}
	return false
}

func (t *TabBar) syncIndicatorSub() tea.Cmd {
	if t.hasAnimatedIndicator() {
		return t.indicatorSub.Start()
	}
	t.indicatorSub.Stop()
	return nil
}

func (t *TabBar) maybeStartReorderAnimation(prevTabs []messages.TabInfo, prevLayouts []tabLayout) (cmd tea.Cmd) {
	// Consume the drag source ID regardless of whether we start an animation,
	// so it never leaks into a subsequent unrelated SetTabs call.
	dragSrcID := t.lastDragSourceID
	dropStartX := t.lastDropStartX
	t.lastDragSourceID = ""
	t.lastDropStartX = 0

	if len(prevTabs) != len(t.tabs) || len(t.tabs) == 0 {
		t.dragAnim.Cancel()
		t.dragOffsetFrom = nil
		t.dragOffsetTo = nil
		t.reorderAnim.Cancel()
		t.reorderOffset = nil
		if t.settlingDrop != nil && !t.hasSessionID(t.settlingDrop.sessionID) {
			t.endSettlingDrop()
		}
		return nil
	}
	prevByID := make(map[string]int, len(prevLayouts))
	for i, tab := range prevTabs {
		prevByID[tab.SessionID] = prevLayouts[i].startCol
	}
	newLayouts := t.computeLayouts()
	offsets := make(map[string]int, len(newLayouts))
	moved := false
	for i, tab := range t.tabs {
		prevStart, ok := prevByID[tab.SessionID]
		if !ok {
			// Session ID mismatch — this is not a pure reorder; cancel any
			// stale animation and bail without starting a new one.
			t.dragAnim.Cancel()
			t.dragOffsetFrom = nil
			t.dragOffsetTo = nil
			t.reorderAnim.Cancel()
			t.reorderOffset = nil
			t.settleAnim.Cancel()
			t.settlingDrop = nil
			return nil
		}
		// The drag source tab should snap to its new position rather than
		// animate from its pre-drag slot (which was visually blank during the
		// drag). Animating it would produce a jarring "pop" back to the old
		// position before sliding to the new one.
		if tab.SessionID == dragSrcID {
			continue
		}
		delta := prevStart - newLayouts[i].startCol
		if delta != 0 {
			moved = true
			offsets[tab.SessionID] = delta
		}
	}
	if !moved && dragSrcID == "" {
		if t.drag.active {
			return nil
		}
		t.dragAnim.Cancel()
		t.dragOffsetFrom = nil
		t.dragOffsetTo = nil
		t.reorderAnim.Cancel()
		t.reorderOffset = nil
		return nil
	}
	if dragSrcID != "" {
		// Bystanders are already at their final positions visually (held there
		// by dragOffsetTo since mouse release). Animating them would snap them
		// back to their old positions and re-animate — wrong. Set nil so
		// reorderVisualOffset returns 0 for everyone; only the settling overlay
		// (driven by reorderAnim below) needs to move.
		t.reorderOffset = nil
	} else {
		t.reorderOffset = offsets
	}
	if dragSrcID != "" {
		// Logical order is already committed. Retarget the one stable-ID overlay
		// from its interpolated on-screen position into the new rendered cell.
		if t.settlingDrop == nil || t.settlingDrop.sessionID != dragSrcID {
			cmd = t.beginSettlingDrop(dragSrcID, float64(dropStartX), float64(dropStartX))
		}
		cmd = tea.Batch(cmd, t.retargetSettlingDrop())
	}
	// Clear drag-preview offsets: the reorder animation handles bystander
	// movement from here, producing a seamless handoff.
	t.dragAnim.Cancel()
	t.dragOffsetFrom = nil
	t.dragOffsetTo = nil
	cmd = tea.Batch(cmd, t.reorderAnim.Start(reorderAnimDuration, animation.EaseOutQuint))
	if dragSrcID != "" && t.settlingDrop != nil && !t.settleAnim.Running() {
		cmd = tea.Batch(cmd, t.settleAnim.Start(reorderAnimDuration, animation.EaseOutQuint))
	}
	return cmd
}

func (t *TabBar) reorderVisualOffset(sessionID string) int {
	if !t.reorderAnim.Running() || len(t.reorderOffset) == 0 {
		return 0
	}
	delta := t.reorderOffset[sessionID]
	if delta == 0 {
		return 0
	}
	return delta - t.reorderAnim.Lerp(0, delta)
}

func (t *TabBar) layoutForSessionID(layouts []tabLayout, sessionID string) (tabLayout, bool) {
	for i, tab := range t.tabs {
		if tab.SessionID == sessionID && i < len(layouts) {
			return layouts[i], true
		}
	}
	return tabLayout{}, false
}

func (t *TabBar) settlingFloatX() float64 {
	if t.settlingDrop == nil {
		return 0
	}
	if !t.settleAnim.Running() {
		return t.settlingDrop.currentX
	}
	return t.settlingDrop.currentX + (t.settlingDrop.targetX-t.settlingDrop.currentX)*t.settleAnim.Value()
}

func (t *TabBar) settlingX() int {
	x := t.settlingFloatX()
	if x >= 0 {
		return int(x + 0.5)
	}
	return int(x - 0.5)
}

func (t *TabBar) beginSettlingDrop(sessionID string, currentX, targetX float64) tea.Cmd {
	t.settleAnim.Cancel()
	t.settlingDrop = &settlingDropState{sessionID: sessionID, currentX: currentX, targetX: targetX}
	if currentX == targetX {
		t.endSettlingDrop()
		return nil
	}
	return t.settleAnim.Start(reorderAnimDuration, animation.EaseOutQuint)
}

func (t *TabBar) captureSettlingPosition() {
	if t.settlingDrop == nil {
		return
	}
	current := t.settlingFloatX()
	t.settleAnim.Cancel()
	t.settlingDrop.currentX = current
}

func (t *TabBar) retargetSettlingDrop() tea.Cmd {
	if t.settlingDrop == nil {
		return nil
	}
	layouts := t.computeLayouts()
	lay, ok := t.layoutForSessionID(layouts, t.settlingDrop.sessionID)
	if !ok {
		t.endSettlingDrop()
		return nil
	}
	viewStart, _, cursor := t.currentTabViewMetrics()
	target := float64(t.clampFloatingX(cursor+lay.startCol-viewStart, lay.tab.Width()))
	if target == t.settlingDrop.targetX && t.settleAnim.Running() {
		return nil
	}
	current := t.settlingFloatX()
	t.settleAnim.Cancel()
	t.settlingDrop.currentX = current
	t.settlingDrop.targetX = target
	if current == target {
		t.endSettlingDrop()
		return nil
	}
	return t.settleAnim.Start(reorderAnimDuration, animation.EaseOutQuint)
}

func (t *TabBar) endSettlingDrop() {
	t.settleAnim.Cancel()
	t.settlingDrop = nil
}

func (t *TabBar) hasSessionID(sessionID string) bool {
	for _, tab := range t.tabs {
		if tab.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (t *TabBar) clampFloatingX(x, tabWidth int) int {
	width := t.width
	return max(0, min(x, max(0, width-tabWidth)))
}

// GetDragLayerInfo returns layer info for the floating dragged tab overlay.
//
// The tab is positioned so that the grab point (where the user originally
// clicked) stays under the cursor, giving the illusion of physically dragging
// the tab. After drop, the same overlay is briefly kept alive and animated
// into the dragged tab's final slot for a smooth handoff.
func (t *TabBar) GetDragLayerInfo(screenWidth, tabBarY int) *DragLayerInfo {
	if t.Height() == 0 || t.width == 0 || screenWidth <= 0 {
		return nil
	}
	if t.drag.active {
		if t.drag.dragIdx < 0 || t.drag.dragIdx >= len(t.tabs) {
			return nil
		}
		layouts := t.computeLayouts()
		if t.drag.dragIdx >= len(layouts) {
			return nil
		}
		tab := layouts[t.drag.dragIdx].tab
		// cursorX is in tab-bar-local coords (EditorHMargin already subtracted).
		// grabOffset keeps the pick-up point anchored under the cursor.
		xPos := max(t.drag.cursorX-t.drag.grabOffset, 0)
		if xPos+tab.Width() > screenWidth {
			xPos = max(0, screenWidth-tab.Width())
		}
		return &DragLayerInfo{Content: ansi.Truncate(tab.View(), screenWidth, ""), X: xPos, Y: tabBarY}
	}

	if t.settlingDrop == nil {
		return nil
	}

	if !t.settleAnim.Running() {
		return nil
	}

	layouts := t.computeLayouts()
	lay, ok := t.layoutForSessionID(layouts, t.settlingDrop.sessionID)
	if !ok {
		return nil
	}
	xPos := max(t.settlingX(), 0)
	if xPos+lay.tab.Width() > screenWidth {
		xPos = max(0, screenWidth-lay.tab.Width())
	}
	return &DragLayerInfo{Content: ansi.Truncate(lay.tab.View(), screenWidth, ""), X: xPos, Y: tabBarY}
}

// recordVisualState checkpoints overlay cells after model changes, independently of View calls.
func (t *TabBar) recordVisualState() {
	t.installGeometry()
	t.hadOverlay = t.settlingDrop != nil || t.drag.active
	switch {
	case t.settlingDrop != nil:
		t.lastOverlayX = t.settlingX()
	case t.drag.active:
		t.lastOverlayX = t.drag.cursorX - t.drag.grabOffset
	default:
		t.lastOverlayX = 0
	}
}
