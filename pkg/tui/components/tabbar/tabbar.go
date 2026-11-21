// Package tabbar provides a horizontal tab bar for the TUI.
package tabbar

import (
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// tabBarHeight is the number of terminal rows the tab bar occupies.
	tabBarHeight = 1
	// fallbackWidth is used when the terminal width is unknown or zero.
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

	// dragThreshold is the minimum mouse movement (in columns) to distinguish
	// a drag from a click.
	dragThreshold = 3

	dragReflowDuration = 200 * time.Millisecond
	tabSettleDuration  = 330 * time.Millisecond
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

// dragState tracks an in-progress tab drag-and-drop operation.
type dragState struct {
	pending bool
	active  bool
	dragIdx int // index of the tab being dragged
	dropIdx int // insertion index (-1 = no valid drop target)
	startX  int // mouse X at drag start
	grab    int // mouse position relative to the left edge of the dragged tab
	cursorX int
}

// isNoOp returns true when the drop would leave the tab in its original position.
func (d dragState) isNoOp() bool {
	return d.dropIdx == noTab || d.dropIdx == d.dragIdx || d.dropIdx == d.dragIdx+1
}

// tabBound records the original (non-shifted) left/right edges of a tab.
type tabBound struct {
	start, end int
}

// TabBar renders a horizontal bar of session tabs with click and keyboard support.
type TabBar struct {
	ar        *animation.Runtime
	tabs      []messages.TabInfo
	activeIdx int
	width     int
	keyMap    KeyMap

	scrollOffset int
	zones        []clickZone

	// Cache the original tab bounds, including off-screen tabs to the right.
	dragBounds []tabBound
	dragOffset int
	dragRight  int
	dragLast   int

	// maxTitleLen is the maximum display length for tab titles.
	// Configurable via user settings; defaults to the constant in tab.go.
	maxTitleLen int

	// lastEnsuredIdx tracks which active tab was last scrolled-to by
	// ensureActiveVisible. This prevents View() from overriding manual
	// scroll actions — ensureActiveVisible only runs when the active tab
	// actually changes.
	lastEnsuredIdx int

	drag   dragState
	motion dragMotion

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
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	return &TabBar{
		ar:             ar,
		keyMap:         DefaultKeyMap(),
		maxTitleLen:    maxTitleLen,
		lastEnsuredIdx: noTab,
		drag:           dragState{dropIdx: noTab},
		motion:         dragMotion{transition: ar.Transition()},
	}
}

// SetCloseTabEnabled enables or disables the close-tab key binding.
// When disabled, Ctrl+W passes through to the editor for word deletion.
func (t *TabBar) SetCloseTabEnabled(v bool) {
	t.keyMap.CloseTab.SetEnabled(v)
}

// SetWidth sets the available width for the tab bar.
func (t *TabBar) SetWidth(width int) {
	if t.width == width {
		if width <= 0 {
			t.cancelDrag()
		}
		return
	}
	t.width = width
	if width <= 0 || t.drag.pending {
		t.cancelDrag()
	}
	t.updateMotionGeometry()
}

// SetMaxTitleLength applies the title truncation limit immediately.
func (t *TabBar) SetMaxTitleLength(maxTitleLen int) {
	if maxTitleLen <= 0 {
		maxTitleLen = defaultMaxTitleLen
	}
	if t.maxTitleLen == maxTitleLen {
		return
	}
	t.maxTitleLen = maxTitleLen
	t.updateMotionGeometry()
	t.lastEnsuredIdx = noTab
	t.clampScroll()
}

// SetTabs updates the list of tabs and active index.
func (t *TabBar) SetTabs(tabs []messages.TabInfo, activeIdx int) tea.Cmd {
	var cmd tea.Cmd
	if !sameOrder(t.tabs, tabs) {
		switch {
		case t.motion.settling && t.motion.matchesReorder(t.tabs, tabs):
			t.motion.applied = true
		case t.IsDragging() || t.motion.settling:
			t.cancelDrag()
			t.zones = t.zones[:0]
		default:
			cmd = t.animateReorder(tabs)
		}
	}
	if activeIdx != t.activeIdx {
		// Active tab changed — force ensureActiveVisible on next View.
		t.lastEnsuredIdx = noTab
	}
	t.tabs = append(t.tabs[:0], tabs...)
	t.activeIdx = activeIdx
	t.clampScroll()
	t.updateMotionGeometry()
	return cmd
}

// Height returns the height of the tab bar.
// Returns 0 when there is a single tab (no bar needed).
func (t *TabBar) Height() int {
	if len(t.tabs) <= 1 {
		return 0
	}
	return tabBarHeight
}

// IsDragging returns true when a tab drag is in progress.
func (t *TabBar) IsDragging() bool {
	return t.drag.pending || t.drag.active
}

// HasFloatingOverlay reports whether the lifted tab is drawn separately.
func (t *TabBar) HasFloatingOverlay() bool { return t.drag.active || t.motion.settling }

// VisualGeneration changes only when rendered tab geometry or drag styling changes.
func (t *TabBar) VisualGeneration() uint64 { return t.visualGeneration }

// StopAnimation cancels any drag or settling motion without reordering.
func (t *TabBar) StopAnimation() { t.cancelDrag() }

// Bindings returns consolidated key bindings for the help bar.
func (t *TabBar) Bindings() []key.Binding {
	return []key.Binding{
		key.NewBinding(
			key.WithKeys("ctrl+t", "ctrl+w"),
			key.WithHelp("Ctrl+t/w", "new/close tab"),
		),
		key.NewBinding(
			key.WithKeys("ctrl+p", "ctrl+n"),
			key.WithHelp("Ctrl+p/n", "prev/next tab"),
		),
	}
}

// Update handles messages and returns commands.
func (t *TabBar) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case animation.TickMsg:
		t.tickMotion(msg)

	case tea.KeyPressMsg:
		switch {
		case msg.Code == tea.KeyEscape && (t.IsDragging() || t.motion.settling):
			t.cancelDrag()
			return nil

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

	case tea.BlurMsg:
		if t.IsDragging() {
			t.cancelDrag()
		}

	case tea.MouseClickMsg:
		if t.HasFloatingOverlay() || t.motion.reordering {
			t.View() // Refresh animated hit zones before a click.
		}
		if t.IsDragging() && msg.Button != tea.MouseLeft {
			return t.handleMouseRelease(t.drag.cursorX)
		}
		if msg.Button == tea.MouseLeft {
			return t.handleLeftClickDown(msg.X)
		}
		if msg.Button == tea.MouseMiddle {
			return t.handleMiddleClick(msg.X)
		}
		return nil

	case tea.MouseMotionMsg:
		return t.handleMouseMotion(msg.X)

	case tea.MouseReleaseMsg:
		if t.IsDragging() {
			return t.handleMouseRelease(msg.X)
		}
	}

	return nil
}

// handleLeftClickDown records a possible drag. Time alone never lifts a tab.
func (t *TabBar) handleLeftClickDown(x int) tea.Cmd {
	for _, z := range slices.Backward(t.zones) {
		if x < z.startX || x >= z.endX {
			continue
		}
		if z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && !z.isClose && !t.motion.reordering {
			if !t.IsDragging() {
				if t.motion.settling && !t.motion.applied && t.motion.from != t.motion.to {
					return t.handleClick(x)
				}
				t.drag = dragState{pending: true, dragIdx: z.tabIdx, dropIdx: noTab, startX: x, cursorX: x, grab: x - z.startX}
			}
			return nil
		}
		break
	}
	return t.handleClick(x)
}

// Click applies a click without acquiring the pointer, for background dialogs.
func (t *TabBar) Click(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseLeft {
		return t.handleClick(msg.X)
	}
	if msg.Button == tea.MouseMiddle {
		return t.handleMiddleClick(msg.X)
	}
	return nil
}

func (t *TabBar) handleMouseMotion(x int) tea.Cmd {
	if !t.IsDragging() {
		return nil
	}
	t.drag.cursorX = x
	if t.drag.pending {
		if abs(x-t.drag.startX) < dragThreshold {
			return nil
		}
		if t.motion.settling {
			t.motion.transition.Cancel()
			t.motion.settling = false
			// Settle positions use the old order; the new drag starts
			// from the committed order and the visible grab point.
			left := t.dragBounds[t.dragOffset].start
			t.cacheTabBounds(t.motionTabs(t.tabs), t.dragOffset, left)
		}
		t.drag.pending, t.drag.active = false, true
		t.motion.lift(t.dragBounds)
		t.visualGeneration++
	}
	before := t.drag.visualDrop()
	t.drag.dropIdx = t.dropIndexForX(x)
	if next := t.floatingX(x); next != t.motion.floatX {
		t.motion.floatX = next
		t.visualGeneration++
	}
	if after := t.drag.visualDrop(); before != after {
		return t.reflow(after)
	}
	return nil
}

func (t *TabBar) handleMouseRelease(x int) tea.Cmd {
	if t.drag.pending {
		t.drag = dragState{dropIdx: noTab}
		return t.handleClick(x)
	}
	if !t.drag.active {
		return nil
	}

	from, to := t.drag.dragIdx, t.drag.dropIdx
	finalTo := from
	if !t.drag.isNoOp() {
		finalTo = to
		if to > from {
			finalTo--
		}
	} else {
		to = from
	}
	cmd := t.settle(x, from, to, finalTo)
	t.drag = dragState{dropIdx: noTab}
	t.visualGeneration++
	if finalTo == from {
		return cmd
	}
	return tea.Batch(core.CmdHandler(messages.ReorderTabMsg{FromIdx: from, ToIdx: finalTo}), cmd)
}

// dropIndexForX uses the lifted tab's edges, not the pointer, against the
// original neighbor midpoints. Moving siblings cannot change the target.
func (t *TabBar) dropIndexForX(x int) int {
	from := t.drag.dragIdx
	source := t.dragBounds[from]
	left, right := x-t.drag.grab, x-t.drag.grab+source.end-source.start
	if left < source.start {
		for i := range from {
			b := t.dragBounds[i]
			if left < (b.start+b.end)/2 {
				return i
			}
		}
	} else {
		for i := len(t.dragBounds) - 1; i > from; i-- {
			b := t.dragBounds[i]
			if right > (b.start+b.end)/2 {
				return i + 1
			}
		}
	}
	return t.drag.dragIdx
}

// handleMiddleClick closes the tab under the cursor on middle-click.
func (t *TabBar) handleMiddleClick(x int) tea.Cmd {
	for _, z := range slices.Backward(t.zones) {
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
	for _, z := range slices.Backward(t.zones) {
		if x < z.startX || x >= z.endX {
			continue
		}

		switch {
		case z.isScrollLeft:
			t.scrollOffset = max(0, t.scrollOffset-1)
			return nil
		case z.isScrollRight:
			t.scrollOffset = min(len(t.tabs)-1, t.scrollOffset+1)
			return nil
		case z.isPlus:
			return core.CmdHandler(messages.SpawnSessionMsg{})
		case z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
			return core.CmdHandler(messages.CloseTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		case z.tabIdx >= 0 && z.tabIdx < len(t.tabs) && z.tabIdx != t.activeIdx:
			return core.CmdHandler(messages.SwitchTabMsg{SessionID: t.tabs[z.tabIdx].SessionID})
		}
		return nil
	}
	return nil
}

// View renders the tab bar as a single line: tab tab tab  +
// Returns empty string when there is a single tab.
func (t *TabBar) View() string {
	if len(t.tabs) <= 1 {
		t.zones = t.zones[:0]
		t.dragBounds = t.dragBounds[:0]
		return ""
	}

	// Reset zones (reuse backing array if available).
	if t.zones != nil {
		t.zones = t.zones[:0]
	}

	availWidth := t.availableWidth()

	// Pre-render all tabs.
	allTabs := make([]Tab, len(t.tabs))
	totalWidth := 0
	now := t.ar.Now()
	for i, info := range t.tabs {
		allTabs[i] = renderTab(info, t.maxTitleLen, dragRoleNone, now)
		totalWidth += allTabs[i].Width()
	}
	totalWidth += plusButtonWidth

	needsScroll := totalWidth > availWidth

	switch {
	case t.IsDragging() || t.motion.settling || t.motion.reordering:
		needsScroll = t.dragOffset > 0 || t.dragLast < len(allTabs)-1
	case !needsScroll:
		// Reset persistent scroll position once everything fits, so the next
		// time the bar overflows again the user starts from the leftmost
		// tab. The mutation lives here, rather than in SetTabs/SetWidth,
		// because only the rendering pass knows the actual tab widths.
		t.scrollOffset = 0 //rubocop:disable Lint/TUIViewPurity // intentional reset; see comment above
	case t.activeIdx != t.lastEnsuredIdx:
		// Only auto-scroll when the active tab changes (e.g. tab switch),
		// not on every render — otherwise manual scroll via arrows is undone.
		t.ensureActiveVisible(allTabs, availWidth)
		t.lastEnsuredIdx = t.activeIdx //rubocop:disable Lint/TUIViewPurity // memo to avoid re-running ensureActiveVisible on every render
	}

	// Compute "+" and arrow colors dynamically from the terminal background.
	chromeFg := styles.MutedContrastFg(styles.Background)
	plusStyle := lipgloss.NewStyle().Foreground(chromeFg)
	arrowStyle := lipgloss.NewStyle().Foreground(chromeFg)
	// Attention arrow style: warning-colored and bold so off-screen attention tabs are obvious.
	attnArrowStyle := lipgloss.NewStyle().Foreground(styles.EnsureContrast(styles.Warning, styles.Background)).Bold(true)

	var line strings.Builder
	var cursor int

	// Left scroll arrow — highlight if any off-screen tab to the left needs attention.
	offset := t.scrollOffset
	if t.IsDragging() || t.motion.settling || t.motion.reordering {
		offset = t.dragOffset
	}
	if needsScroll && offset > 0 {
		style := arrowStyle
		if t.hasAttentionInRange(0, offset) {
			style = attnArrowStyle
		}
		arrow := style.Render(scrollLeftText)
		line.WriteString(arrow)
		t.zones = append(t.zones, clickZone{
			startX: cursor, endX: cursor + scrollArrowWidth,
			tabIdx: noTab, isScrollLeft: true,
		})
		cursor += scrollArrowWidth
	}

	lastVisibleIdx := offset - 1
	if t.HasFloatingOverlay() || t.motion.reordering {
		line.WriteString(t.renderMotion(allTabs))
		cursor = t.dragRight
		lastVisibleIdx = t.dragLast
	} else {
		if !t.IsDragging() {
			t.cacheTabBounds(allTabs, offset, cursor)
		}
		for i := offset; i < len(allTabs); i++ {
			tab := allTabs[i]
			tabW := tab.Width()

			// Reserve space for the possible right arrow and "+" button.
			rightReserve := plusButtonWidth
			if needsScroll && i < len(allTabs)-1 {
				rightReserve += scrollArrowWidth
			}
			if cursor+tabW+rightReserve > availWidth && i > offset {
				break
			}
			t.addTabZones(i, tab, cursor, cursor+tabW)
			line.WriteString(tab.View())
			cursor += tabW
			lastVisibleIdx = i
		}
		if !t.IsDragging() {
			t.cacheVisibleBounds(lastVisibleIdx, cursor)
		}
	}

	// Right scroll arrow — highlight if any off-screen tab to the right needs attention.
	if needsScroll && lastVisibleIdx < len(allTabs)-1 {
		style := arrowStyle
		if t.hasAttentionInRange(lastVisibleIdx+1, len(t.tabs)) {
			style = attnArrowStyle
		}
		arrow := style.Render(scrollRightText)
		line.WriteString(arrow)
		t.zones = append(t.zones, clickZone{
			startX: cursor, endX: cursor + scrollArrowWidth,
			tabIdx: noTab, isScrollRight: true,
		})
		cursor += scrollArrowWidth
	}

	// "+" button.
	plus := plusStyle.Render(plusButtonText)
	line.WriteString(plus)
	t.zones = append(t.zones, clickZone{
		startX: cursor, endX: cursor + plusButtonWidth,
		tabIdx: noTab, isPlus: true,
	})

	// Hit testing must use the same topmost overlay geometry as the compositor.
	if overlay := t.FloatingOverlay(); overlay != nil {
		t.addTabZones(overlay.index, overlay.tab, overlay.X, t.availableWidth())
	}
	return line.String()
}

// ensureActiveVisible adjusts scrollOffset so the active tab is visible.
func (t *TabBar) ensureActiveVisible(tabs []Tab, availWidth int) {
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}

	for t.scrollOffset < t.activeIdx {
		usedWidth := 0
		if t.scrollOffset > 0 {
			usedWidth += scrollArrowWidth
		}

		fits := false
		for i := t.scrollOffset; i < len(tabs); i++ {
			usedWidth += tabs[i].Width()

			if i == t.activeIdx {
				rightReserve := plusButtonWidth
				if i < len(tabs)-1 {
					rightReserve += scrollArrowWidth
				}
				if usedWidth+rightReserve <= availWidth {
					fits = true
				}
				break
			}
		}

		if fits {
			break
		}
		t.scrollOffset++
	}
}

// hasAttentionInRange returns true if any tab in [start, end) needs attention.
func (t *TabBar) hasAttentionInRange(start, end int) bool {
	for i := start; i < end && i < len(t.tabs); i++ {
		if t.tabs[i].NeedsAttention {
			return true
		}
	}
	return false
}

// clampScroll ensures scrollOffset is within valid bounds.
func (t *TabBar) clampScroll() {
	if t.scrollOffset >= len(t.tabs) {
		t.scrollOffset = max(0, len(t.tabs)-1)
	}
	if t.activeIdx < t.scrollOffset {
		t.scrollOffset = t.activeIdx
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
