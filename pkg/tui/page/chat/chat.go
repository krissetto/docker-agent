package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/docker/docker-agent/pkg/tui/widgets/help"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuibanner "github.com/docker/docker-agent/pkg/tui/banner"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/subagentview"
)

const (
	// minWindowWidth is the threshold below which sidebar switches to horizontal mode
	minWindowWidth = 120
	// dragThreshold is pixels of movement needed to distinguish click from drag
	dragThreshold = 3
	// toggleColumnWidth is the width of the sidebar toggle/resize handle column
	toggleColumnWidth = 1
	// appPaddingHorizontal is total horizontal padding from AppStyle (left + right)
	appPaddingHorizontal = 2 * styles.AppPadding
)

// sidebarLayoutMode represents how the sidebar is displayed
type sidebarLayoutMode int

const (
	// sidebarVertical: wide window, sidebar beside the chat (left or right)
	sidebarVertical sidebarLayoutMode = iota
	// sidebarCollapsed: wide window but user collapsed sidebar, shown at top with toggle
	sidebarCollapsed
	// sidebarCollapsedNarrow: narrow window or forced band position, shown without toggle
	sidebarCollapsedNarrow
)

// sidebarLayout holds computed layout values for the current frame.
// Computing this once per update avoids repeating calculations across View, SetSize, and input handlers.
type sidebarLayout struct {
	mode          sidebarLayoutMode
	sidebarOnLeft bool // vertical mode: sidebar sits left of the chat
	bandAtBottom  bool // collapsed modes: band sits below the chat instead of above
	innerWidth    int  // window width minus app padding
	chatWidth     int  // width available for chat/messages
	chatStartX    int  // X coordinate where the chat area starts (relative to innerWidth)
	sidebarWidth  int  // actual sidebar width (varies by mode)
	sidebarStartX int  // X coordinate where sidebar content starts (relative to innerWidth)
	handleX       int  // X coordinate of resize handle column (only valid in vertical mode)
	chatHeight    int  // height available for chat area
	sidebarHeight int  // height of sidebar
}

// isOnHandle returns true if adjustedX (already adjusted for app padding) is on the resize handle.
func (l sidebarLayout) isOnHandle(adjustedX int) bool {
	return l.mode == sidebarVertical && adjustedX == l.handleX
}

// isInSidebar returns true if adjustedX is within the sidebar area.
func (l sidebarLayout) isInSidebar(adjustedX int) bool {
	if l.mode != sidebarVertical {
		return false
	}
	if l.sidebarOnLeft {
		return adjustedX < l.handleX
	}
	return adjustedX >= l.sidebarStartX
}

// bandY returns the Y coordinate where the collapsed band starts.
func (l sidebarLayout) bandY() int {
	if l.bandAtBottom {
		return l.chatHeight
	}
	return 0
}

// isInBand returns true if y falls within the collapsed sidebar band.
func (l sidebarLayout) isInBand(y int) bool {
	if l.mode == sidebarVertical {
		return false
	}
	return y >= l.bandY() && y < l.bandY()+l.sidebarHeight
}

// bandContentY converts a screen Y coordinate to a band-local content row.
func (l sidebarLayout) bandContentY(y int) int {
	return y - l.bandY()
}

// showToggle returns true if a toggle glyph should be shown.
func (l sidebarLayout) showToggle() bool {
	return l.mode == sidebarVertical || l.mode == sidebarCollapsed
}

// SidebarSettings holds the sidebar display settings that should persist across session changes.
type SidebarSettings struct {
	Collapsed      bool
	PreferredWidth int
}

// Page represents the main chat content area (messages + sidebar).
// The editor and resize handle are owned by the parent (tui.Model).
type Page interface {
	layout.Model
	layout.Sizeable
	layout.Help
	CompactSession(additionalPrompt string) tea.Cmd
	// SetSessionStarred updates the sidebar star indicator
	SetSessionStarred(starred bool)
	// SetTitleRegenerating sets the title regenerating state on the sidebar
	SetTitleRegenerating(regenerating bool) tea.Cmd
	// ScrollToBottom scrolls the messages viewport to the bottom if auto-scroll is active.
	ScrollToBottom() tea.Cmd
	// IsWorking returns whether the agent is currently working
	IsWorking() bool
	// IsInlineEditing returns true if a past user message is being edited inline
	IsInlineEditing() bool
	// IsTitleEditing reports the sidebar title input owns keyboard and paste.
	IsTitleEditing() bool
	// IsSelecting returns true while a text-selection drag is active in the messages panel
	IsSelecting() bool
	// QueueLength returns the number of queued messages
	QueueLength() int
	// FocusMessages gives focus to the messages panel for keyboard scrolling
	FocusMessages() tea.Cmd
	// FocusMessageAt gives focus and selects the message at the given screen coordinates
	FocusMessageAt(x, y int) tea.Cmd
	// BlurMessages removes focus from the messages panel
	BlurMessages()
	// GetSidebarSettings returns the current sidebar display settings
	GetSidebarSettings() SidebarSettings
	// SetSidebarSettings applies sidebar display settings
	SetSidebarSettings(settings SidebarSettings)
	// SetLayoutSettings applies layout customization (sidebar position,
	// section spacing, section visibility, and agent info mode) and
	// relayouts the page.
	SetLayoutSettings(settings msgtypes.LayoutSettings) tea.Cmd
	// SetSendMode sets what happens to messages sent while the agent is
	// working: steer into the ongoing stream or queue until the turn ends.
	SetSendMode(mode msgtypes.SendMode)
	// SetInterruptMode sets how Esc interrupts a running stream.
	SetInterruptMode(mode msgtypes.InterruptMode)
	// SetShowBanner controls whether the ASCII-art startup banner is drawn
	// on an empty conversation.
	SetShowBanner(show bool)
	// SetRoutingID records the tab identity used to address this page's
	// one-shot UI timers back to it (messages.RoutedMsg.SessionID). The
	// appModel keys its chat pages — and the supervisor its event routing —
	// by this ID, which is the tab's initial session ID and may diverge from
	// the current app.Session().ID after a session restore or in-place
	// replace.
	SetRoutingID(id string)
	// TakeRoutedTimers returns and clears the routed one-shot commands
	// (presentation timers, generated-media resolution) armed by the most
	// recent Update. The active page's Update already returns them inside
	// its regular command; the appModel calls this for background pages —
	// whose regular commands are discarded — so those deadlines and
	// resolutions keep running while a tab is hidden.
	TakeRoutedTimers() tea.Cmd
	VisualGeneration() uint64
}

func (p *chatPage) VisualGeneration() uint64 { return p.messages.VisualGeneration() }

func (p *chatPage) SidebarVisualGeneration() uint64 {
	return p.sidebar.VisualGeneration() + p.shellVisualGeneration
}

type sidebarClick struct {
	sessionID string
	target    MouseTarget
	turnID    string
	at        time.Time
}

type queuedMessage struct {
	turnID  string
	content string
}

// chatPage implements Page
//
//nolint:gocritic // Kept near its supporting queued-message declarations.
type chatPage struct {
	ar            *animation.Runtime
	width, height int

	// Components
	sidebar   sidebar.Model
	messages  messages.Model
	subagents *subagentindex.Index

	sessionState *service.SessionState

	lastSidebarClick sidebarClick

	// State
	working              bool
	leanMode             bool
	hideSidebar          bool
	layoutSettings       msgtypes.LayoutSettings
	splitSidebarSettings *SidebarSettings
	sendMode             msgtypes.SendMode

	msgCancel       context.CancelFunc
	cancel          context.CancelFunc
	streamCancelled bool
	// lifecycle owns nested stream and pending-turn transitions.
	lifecycle   lifecycle.State
	inputReplay lifecycle.InputReplay
	// ownedSkillOperation is the exact operation admitted by this page.
	// Foreign journal lifecycle events never affect its busy boundary.
	ownedSkillOperation string
	ownedSkillStream    bool
	teamAgentNames      []string // last team roster; a change re-renders cached messages with fresh agent colors
	streamStartTime     time.Time

	// routingID is the tab identity this page's routed UI timers are
	// addressed to; empty for standalone pages (timers then fire unrouted,
	// which is correct when this is the only page).
	routingID string
	// pendingTimers holds the routed one-shot commands (presentation timers,
	// generated-media resolution) armed by the current Update, so they can be
	// re-collected via TakeRoutedTimers when the regular command is discarded
	// (background tabs).
	pendingTimers []tea.Cmd

	// Attach protocol state (tabs attached to a live subagent session).
	// snapshotEnd is the session item count captured when the transcript
	// snapshot was rendered: position-stamped events below it are already in
	// the snapshot and are dropped. The head of an in-flight assistant
	// message arrives as seed events from the session event hub (captured
	// atomically with the subscription), so nothing needs repairing after
	// the fact — and no scroll-disturbing rebuild is needed.
	snapshotEnd      int
	replay           *pageReplay
	replayGeneration uint64
	asyncReplay      bool

	// Track whether we've received content from an assistant response
	// Used by --exit-after-response to ensure we don't exit before receiving content
	hasReceivedAssistantContent bool
	showStartupBanner           bool
	// hideBanner mirrors the user's show_banner setting; the zero value
	// keeps the banner so page literals stay banner-enabled.
	hideBanner bool

	// Canonical session mailbox projection. Entries are added and removed only
	// by accepted/promoted envelopes (or a replacement snapshot).
	messageQueue []queuedMessage

	// Editing state for branching sessions
	editing          bool
	branchAtPosition int
	editAttachments  []msgtypes.Attachment // Preserved attachments from original message

	// Key map
	keyMap KeyMap

	// interruptMode controls how Esc interrupts a running stream.
	interruptMode msgtypes.InterruptMode

	ctx func() context.Context

	app              *app.App
	sharedProjection bool

	// Command parser for handling slash commands in the editor
	commandParser *commands.Parser

	// Sidebar drag state
	isDraggingSidebar     bool // True while dragging the sidebar resize handle
	sidebarDragStartX     int  // X position when drag started
	sidebarDragStartWidth int  // Sidebar preferred width when drag started
	sidebarDragMoved      bool // True if mouse moved beyond threshold during drag

	// appliedLayout is the layout geometry last pushed to child components by
	// SetSize. Update compares it against the live layout to catch silent
	// shifts (e.g. the collapsed sidebar band growing when async startup info
	// arrives) that would otherwise leave mouse hit-testing offset.
	appliedLayout sidebarLayout

	splitPresentation      *SplitPresentationGeometry
	transcriptPresentation presentationCache
	presentationHidden     bool
	shellVisualGeneration  uint64
}

// sidebarHidden reports whether the sidebar should be omitted entirely from
// layout and rendering (lean mode or explicit --sidebar=false).
func (p *chatPage) sidebarHidden() bool {
	return p.leanMode || p.hideSidebar
}

// computeSidebarLayout calculates the layout based on current state.
func (p *chatPage) computeSidebarLayout() sidebarLayout {
	return p.computeSidebarLayoutForSize(p.width, p.height)
}

func (p *chatPage) computeSidebarLayoutForSize(width, height int) sidebarLayout {
	innerWidth := max(0, width-appPaddingHorizontal)

	// No sidebar at all (lean mode or hideSidebar): chat fills the area.
	if p.sidebarHidden() {
		return sidebarLayout{
			mode:       sidebarCollapsedNarrow,
			innerWidth: innerWidth,
			chatWidth:  innerWidth,
			chatHeight: max(1, height),
		}
	}

	position := p.layoutSettings.SidebarPosition
	sideBySide := position == msgtypes.SidebarLeft || position == "" || position == msgtypes.SidebarRight

	var mode sidebarLayoutMode
	switch {
	case sideBySide && width >= minWindowWidth && !p.presentationSidebarSettings().Collapsed:
		mode = sidebarVertical
	case sideBySide && width >= minWindowWidth:
		mode = sidebarCollapsed
	default:
		mode = sidebarCollapsedNarrow
	}

	l := sidebarLayout{
		mode:          mode,
		innerWidth:    innerWidth,
		sidebarOnLeft: position == msgtypes.SidebarLeft,
		bandAtBottom:  position == msgtypes.SidebarBottom,
	}

	switch mode {
	case sidebarVertical:
		l.sidebarWidth = p.sidebar.ClampWidth(p.presentationSidebarSettings().PreferredWidth, innerWidth)
		l.chatWidth = max(1, innerWidth-l.sidebarWidth)
		if l.sidebarOnLeft {
			l.sidebarStartX = 0
			l.handleX = l.sidebarWidth - toggleColumnWidth
			l.chatStartX = l.sidebarWidth
		} else {
			l.handleX = l.chatWidth
			l.sidebarStartX = l.chatWidth + toggleColumnWidth
		}
		l.chatHeight = max(1, height)
		l.sidebarHeight = l.chatHeight

	case sidebarCollapsed:
		l.sidebarWidth = innerWidth - toggleColumnWidth
		l.chatWidth = innerWidth
		l.sidebarHeight = p.sidebar.CollapsedHeight(l.sidebarWidth)
		l.chatHeight = max(1, height-l.sidebarHeight)

	case sidebarCollapsedNarrow:
		l.sidebarWidth = innerWidth
		l.chatWidth = innerWidth
		l.sidebarHeight = p.sidebar.CollapsedHeight(l.sidebarWidth)
		l.chatHeight = max(1, height-l.sidebarHeight)
	}

	return l
}

// KeyMap defines key bindings for the chat page
type KeyMap struct {
	Cancel          key.Binding
	ToggleSplitDiff key.Binding
	ToggleSidebar   key.Binding
}

// defaultKeyMap returns the default key bindings.
// ctrl+t is reserved for "new tab" in the tab bar,
// so ToggleSplitDiff is disabled (available via /split-diff command instead).
func defaultKeyMap() KeyMap {
	splitDiff := key.NewBinding(
		key.WithKeys("ctrl+t"),
		key.WithHelp("Ctrl+t", "toggle split diff"),
	)
	splitDiff.SetEnabled(false)

	return KeyMap{
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("Esc", "interrupt"),
		),
		ToggleSplitDiff: splitDiff,
		ToggleSidebar:   core.GetKeys().ToggleSidebar,
	}
}

// New creates a new chat page
func New(ar *animation.Runtime, ctx context.Context, a *app.App, sessionState *service.SessionState, opts ...PageOption) Page {
	if a == nil {
		panic("chat page requires a session-backed app")
	}
	pageCtx, cancel := context.WithCancel(ctx)
	index := subagentindex.New()
	p := &chatPage{
		ar:                ar,
		cancel:            cancel,
		ctx:               func() context.Context { return context.WithoutCancel(ctx) },
		sidebar:           sidebar.New(ar, pageCtx, sessionState),
		messages:          messages.New(ar, sessionState, index),
		subagents:         index,
		app:               a,
		keyMap:            defaultKeyMap(),
		commandParser:     commands.NewParser(),
		sessionState:      sessionState,
		showStartupBanner: true,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// PageOption configures a chat page.
type PageOption func(*chatPage)

// WithLeanMode creates a lean chat page with no sidebar.
func WithLeanMode() PageOption {
	return func(p *chatPage) {
		p.leanMode = true
	}
}

// WithHideSidebar hides the sidebar without enabling lean mode.
// The sidebar cannot be re-shown via the TUI.
func WithHideSidebar() PageOption {
	return func(p *chatPage) {
		p.hideSidebar = true
		p.keyMap.ToggleSidebar.SetEnabled(false)
	}
}

// WithShowBanner controls whether the ASCII-art startup banner is drawn on
// an empty conversation.
func WithShowBanner(show bool) PageOption {
	return func(p *chatPage) {
		p.hideBanner = !show
	}
}

// WithCommandParser injects a command parser for handling slash commands in the editor.
func WithCommandParser(p *commands.Parser) PageOption {
	return func(cp *chatPage) {
		cp.commandParser = p
	}
}

// WithLayoutSettings applies initial layout customization (sidebar position,
// section spacing, section visibility, agent info mode, and agent filtering).
func WithLayoutSettings(settings msgtypes.LayoutSettings) PageOption {
	return func(p *chatPage) {
		p.layoutSettings = settings
		p.sidebar.SetSectionVisibility(sectionVisibility(settings))
		p.sidebar.SetSectionGap(settings.SectionSpacing.BlankLines())
		p.sidebar.SetAgentInfoMode(agentInfoMode(settings.SidebarInfoMode))
		p.sidebar.SetActiveAgentsOnly(settings.ActiveAgentsOnly)
	}
}

// WithSendMode sets the initial behavior of messages sent while the agent
// is working: steer into the ongoing stream or queue until the turn ends.
func WithSendMode(mode msgtypes.SendMode) PageOption {
	return func(p *chatPage) {
		p.sendMode = mode
	}
}

func WithInterruptMode(mode msgtypes.InterruptMode) PageOption {
	return func(p *chatPage) {
		p.interruptMode = mode
	}
}

// sectionVisibility maps layout settings to the sidebar's visibility config.
func sectionVisibility(settings msgtypes.LayoutSettings) sidebar.SectionVisibility {
	return sidebar.SectionVisibility{
		HideSessionPath: settings.HideSessionPath,
		HideUsage:       settings.HideUsage,
		HideAgents:      settings.HideAgents,
		HideTools:       settings.HideTools,
		HideTodos:       settings.HideTodos,
	}
}

// agentInfoMode maps the layout's sidebar info mode to the sidebar's
// agent-roster renderer selection; empty/unknown values fall back to compact.
func agentInfoMode(mode msgtypes.SidebarInfoMode) sidebar.AgentInfoMode {
	if msgtypes.ParseSidebarInfoMode(string(mode)) == msgtypes.InfoModeDetailed {
		return sidebar.AgentInfoDetailed
	}
	return sidebar.AgentInfoCompact
}

// Init initializes the chat page
func (p *chatPage) Init() tea.Cmd {
	if p.asyncReplay && p.app.Session() != nil {
		return tea.Batch(p.messages.Init(), p.beginReplay(runtime.SessionSnapshot{Session: p.app.Session(), TranscriptPosition: p.app.Session().ItemCount()}))
	}

	var cmds []tea.Cmd

	cmds = append(cmds, p.messages.Init())

	// Load state from existing session (for session restore and branching)
	if sess := p.app.Session(); sess != nil {
		p.inputReplay.Reset(sess)
		cmds = append(cmds, p.hydrateSidebarSession(sess))
		if len(sess.Messages) > 0 {
			restoredMedia, mediaRequests := p.collectRestoredGeneratedMedia(sess)
			cmds = append(cmds, p.messages.LoadFromSession(sess, restoredMedia))
			if resolve := p.resolveGeneratedMediaCmd(mediaRequests); resolve != nil {
				cmds = append(cmds, resolve)
			}
		}
		// Attached subagent tab: root the swarm section at the subagent's own
		// node and show the clickable link back to the parent tab. snapshotEnd
		// is the rendered snapshot's length (taken from the same item copy):
		// position-stamped events below it are already on screen and settle or
		// drop; events at/after it apply. Attaching mid-run needs no special
		// working-state handling here: the session event hub seeds a synthetic
		// StreamStarted for a live run, driving the spinner through the normal
		// path (here and in the tab bar).
		// The tree snapshot is synchronous and authoritative, so it also closes
		// the race where the running tree event predates tab attachment.
		if p.app.AttachedSubagent() != nil {
			p.snapshotEnd = p.messages.LoadedItemCount()
		}
	}

	return tea.Batch(cmds...)
}

// hydrateSidebarSession uses only topology belonging to this exact session/root.
func (p *chatPage) hydrateSidebarSession(sess *session.Session) tea.Cmd {
	if sess == nil {
		return nil
	}
	rootID := subagent.SessionRootID(sess.ID)
	if info := p.app.AttachedSubagent(); info != nil {
		p.sidebar.SetSubagentContext(info.NodeID, info.ParentAgent, info.ParentSessionID)
		rootID = info.NodeID
	} else {
		p.sidebar.SetSubagentContext("", "", "")
	}
	snapshot := sess.GetSubagentTree()
	if snapshot == nil {
		if current := p.app.Session(); current != nil && current.ID == sess.ID {
			snapshot = current.GetSubagentTree()
		}
	}
	if provider, ok := p.app.Runtime().(interface{ SubagentTree() *subagent.Tree }); ok && provider.SubagentTree() != nil && rootID != "" {
		live := provider.SubagentTree().Snapshot()
		if _, found := subagentview.Find(live.Nodes, rootID); found {
			snapshot = &live
		}
	}
	hydrated := sess.Clone()
	hydrated.SetSubagentTree(nil)
	if snapshot != nil && rootID != "" {
		if _, found := subagentview.Find(snapshot.Nodes, rootID); found {
			hydrated.SetSubagentTree(snapshot)
		}
	}
	p.sidebar.LoadFromSession(hydrated)
	p.subagents.Clear()
	if tree := hydrated.GetSubagentTree(); tree != nil {
		p.subagents.Reset(*tree)
		return p.forwardToSidebar(runtime.SubagentTree(*tree))
	}
	return nil
}

func WatchGitBranch(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		return p.sidebar.Init()
	}
	return nil
}

// ClearSidebarHover fades occluded pointer targets while the page remains visible.
func ClearSidebarHover(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		p.lastSidebarClick = sidebarClick{}
		return tea.Batch(p.sidebar.ClearSubagentHover(), p.messages.ClearReferenceHover())
	}
	return nil
}

// SetSidebarPresentationActive prevents hidden updates from acquiring finite animation leases.
func SetSidebarPresentationActive(page Page, active bool) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		p.messages.SetReferencePresentationActive(active && !p.presentationHidden)
		active = active && p.sidebarInteractive()
		if !active {
			p.lastSidebarClick = sidebarClick{}
		}
		if owner, ok := p.sidebar.(interface{ SetPresentationActive(active bool) tea.Cmd }); ok {
			return owner.SetPresentationActive(active)
		}
	}
	if !active {
		CancelSidebarPresentation(page)
	}
	return nil
}

// CancelSidebarPresentation releases finite presentation animations when a page is hidden.
func CancelSidebarPresentation(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.messages.CancelReferenceHover()
		switch presentation := p.sidebar.(type) {
		case interface{ CancelPresentation() }:
			presentation.CancelPresentation()
		case interface{ CancelHover() }:
			presentation.CancelHover()
		}
	}
}

func Cleanup(page Page) {
	if p, ok := page.(*chatPage); ok {
		animation.StopView(p.sidebar)
		p.cancel()
		p.subagents.Clear()
	}
}

// Update handles messages and updates the page state
func (p *chatPage) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseWheelMsg, msgtypes.WheelCoalescedMsg:
		p.sidebar.ResetTodoClick()
	}

	if snapshot, ok := msg.(msgtypes.TodosSnapshotMsg); ok {
		return p, p.forwardToSidebar(snapshot)
	}
	p.pendingTimers = nil
	defer p.stopHiddenPresentation()
	var model layout.Model = p
	handled, cmd := p.updateReplay(msg)
	if !handled {
		model, cmd = p.update(msg)
	}
	// State changes (async sidebar updates, streaming indicators) can move
	// child components without any resize. Child positions are only applied
	// in SetSize, so reapply the geometry when the live layout drifted from
	// the last applied one; otherwise mouse hit-testing stays offset until
	// the next window resize.
	if relayout := p.relayoutIfNeeded(); relayout != nil {
		cmd = tea.Batch(cmd, relayout)
	}
	return model, tea.Batch(cmd, p.reconcileSidebarLayout())
}

func (p *chatPage) reconcileSidebarLayout() tea.Cmd {
	if owner, ok := p.sidebar.(interface{ ReconcileLayout() tea.Cmd }); ok {
		return owner.ReconcileLayout()
	}
	return nil
}

// relayoutIfNeeded reapplies the current geometry when the computed layout no
// longer matches the one last pushed to child components.
func (p *chatPage) relayoutIfNeeded() tea.Cmd {
	if p.width <= 0 || p.height <= 0 {
		return nil
	}
	if p.computeSidebarLayout() == p.appliedLayout {
		return nil
	}
	return p.SetSize(p.width, p.height)
}

func (p *chatPage) update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Timers armed by a previous Update were dispatched by its caller (either
	// through the returned command or via TakeRoutedTimers); only this
	// update's timers may be collected after it.
	p.pendingTimers = nil
	if p.IsMessagesScrollbarDragging() {
		switch msg.(type) {
		case tea.MouseMotionMsg, tea.MouseReleaseMsg:
			// Pointer capture precedes sidebar/split hit testing, including when
			// the root routes coordinates outside this page's rectangle.
			model, cmd := p.messages.Update(msg)
			p.messages = model.(messages.Model)
			if _, motion := msg.(tea.MouseMotionMsg); motion {
				cmd = tea.Batch(cmd, p.sidebar.ClearSubagentHover())
			}
			return p, cmd
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := p.SetSize(msg.Width, msg.Height)
		return p, cmd

	case tea.PasteMsg:
		if p.sidebarInteractive() && p.sidebar.IsEditingTitle() {
			return p, p.sidebar.UpdateTitleInput(msg)
		}
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		return p, cmd

	case tea.KeyPressMsg:
		return p.handleKeyPress(msg)

	case tea.MouseClickMsg:
		return p.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return p.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return p.handleMouseRelease(msg)

	case msgtypes.WheelCoalescedMsg:
		return p.handleWheelCoalesced(msg)

	case msgtypes.StreamCancelledMsg:
		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)

		// Forward to sidebar to stop its spinners
		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)

		var cmds []tea.Cmd
		cmds = append(cmds, cmd, sidebarCmd)

		if msg.ShowMessage {
			cmds = append(cmds, p.messages.AddCancelledMessage())
		}
		cmds = append(cmds, p.messages.ScrollToBottom())

		return p, tea.Batch(cmds...)

	case msgtypes.EditUserMessageMsg:
		return p.handleEditUserMessage(msg)

	case messages.InlineEditCommittedMsg:
		return p.handleInlineEditCommitted(msg)

	case messages.InlineEditCancelledMsg:
		return p.handleInlineEditCancelled(msg)

	case msgtypes.SendMsg:
		slog.Debug(msg.Content)
		return p.handleSendMsg(msg)

	case skillAdmissionMsg:
		if msg.operationID != p.ownedSkillOperation {
			return p, nil
		}
		if msg.err != nil {
			p.ownedSkillOperation = ""
			p.ownedSkillStream = false
			return p, tea.Batch(p.finishSkillOperation(), notification.ErrorCmd("Could not start fork skill: "+msg.err.Error()))
		}
		return p, nil

	case steerSentMsg:
		return p, notification.InfoCmd("Message sent to the working agent · /settings to queue instead")

	case steerFailedMsg:
		return p, notification.ErrorCmd(fmt.Sprintf("Could not attach the message to the running stream: %v", msg.err))

	case followUpSentMsg:
		return p, notification.InfoCmd("Follow-up queued for the next turn")

	case followUpFailedMsg:
		return p, notification.ErrorCmd(fmt.Sprintf("Could not enqueue the follow-up: %v", msg.err))

	case msgtypes.RetryMsg:
		return p.handleRetry()

	case msgtypes.ToggleHideToolResultsMsg:
		// Forward to messages component to invalidate cache and trigger redraw
		model, cmd := p.messages.Update(messages.ToggleHideToolResultsMsg{})
		p.messages = model.(messages.Model)
		return p, cmd

	case msgtypes.ClearQueueMsg:
		return p.handleClearQueue()

	case msgtypes.RestorePendingMessagesMsg:
		return p, nil

	case generatedMediaResolvedMsg:
		return p, p.messages.UpdateAssistantMedia(msg.media)

	case msgtypes.ThemeChangedMsg:
		// Theme changed - forward to all child components to invalidate caches
		var cmds []tea.Cmd

		model, cmd := p.messages.Update(msg)
		p.messages = model.(messages.Model)
		cmds = append(cmds, cmd)

		// Forward to sidebar to ensure it picks up new theme colors
		sidebarModel, sidebarCmd := p.sidebar.Update(msg)
		p.sidebar = sidebarModel.(sidebar.Model)
		cmds = append(cmds, sidebarCmd)

		return p, tea.Batch(cmds...)

	default:
		// Try to handle as a runtime event
		if handled, cmd := p.handleRuntimeEvent(msg); handled {
			return p, cmd
		}
	}

	sidebarModel, sidebarCmd := p.sidebar.Update(msg)
	p.sidebar = sidebarModel.(sidebar.Model)

	chatModel, chatCmd := p.messages.Update(msg)
	p.messages = chatModel.(messages.Model)

	return p, tea.Batch(sidebarCmd, chatCmd)
}

func (p *chatPage) setWorking(working bool) tea.Cmd {
	wasWorking := p.working
	p.working = working

	if working != wasWorking {
		return core.CmdHandler(msgtypes.WorkingStateChangedMsg{
			Working:     working,
			QueueLength: len(p.messageQueue),
		})
	}

	return nil
}

// setPendingResponse adds or removes the pending-response spinner message
// inside the messages component. When starting, it adds a spinner message to
// the scrollable list; when stopping, it explicitly removes any lingering spinner.
func (p *chatPage) setPendingResponse(pending bool) tea.Cmd {
	if pending {
		sender, label := p.pendingSpinnerContext()
		return p.messages.AddAssistantMessage(sender, label)
	}
	p.messages.RemoveSpinner()
	return nil
}

// pendingSpinnerContext labels the waiting spinner during delegation only.
// Depth < 2 → empty (default playful spinner); nested → child + "parent → child".
func (p *chatPage) pendingSpinnerContext() (sender, label string) {
	n := len(p.lifecycle.Streams)
	if n < 2 {
		return "", ""
	}
	child := p.lifecycle.Streams[n-1].AgentName
	return child, p.lifecycle.Streams[n-2].AgentName + " → " + child
}

// renderCollapsedSidebar renders the sidebar in collapsed/band mode.
func (p *chatPage) renderCollapsedSidebar(sl sidebarLayout) string {
	// Guard against unset/invalid layout (can happen before WindowSizeMsg is received).
	width := max(0, sl.innerWidth)
	height := max(0, sl.sidebarHeight)
	if width == 0 || height == 0 {
		return ""
	}

	sidebarView := p.sidebar.View()
	sidebarLines := strings.Split(sidebarView, "\n")

	// Place toggle glyph at the far right of the first line
	if sl.showToggle() && sl.mode != sidebarVertical && len(sidebarLines) > 0 {
		toggleGlyph := styles.MutedStyle.Render("«")
		glyphW := lipgloss.Width(toggleGlyph)
		padded := lipgloss.NewStyle().Width(width - glyphW).Render(sidebarLines[0])
		sidebarLines[0] = padded + toggleGlyph
	}

	if len(sidebarLines) > height {
		sidebarLines = sidebarLines[:height]
	}

	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		Align(lipgloss.Left, lipgloss.Top).
		Render(strings.Join(sidebarLines, "\n"))
}

func (p *chatPage) messagesView(sl sidebarLayout) string {
	messagesView := p.messages.View()
	if messagesView != "" || !p.showStartupBanner || p.hideBanner {
		return messagesView
	}
	if sl.chatWidth < tuibanner.Width || sl.chatHeight < tuibanner.Height {
		return ""
	}

	banner := styles.BaseStyle.Foreground(styles.Accent).Render(strings.Join(tuibanner.Lines, "\n"))
	return lipgloss.Place(
		sl.chatWidth,
		sl.chatHeight,
		lipgloss.Center,
		lipgloss.Center,
		banner,
	)
}

// View renders the chat page (messages + sidebar only, no editor or resize handle)
func (p *chatPage) View() string {
	if p.splitPresentation != nil {
		return p.TranscriptView()
	}
	sl := p.computeSidebarLayout()

	messagesView := p.messagesView(sl)

	var bodyContent string

	switch sl.mode {
	case sidebarVertical:
		chatView := styles.ChatStyle.
			Height(sl.chatHeight).
			Width(sl.chatWidth).
			Render(messagesView)

		toggleCol := p.renderSidebarHandle(sl.chatHeight)

		sidebarView := lipgloss.NewStyle().
			Width(sl.sidebarWidth-toggleColumnWidth).
			Height(sl.chatHeight).
			Align(lipgloss.Left, lipgloss.Top).
			Render(p.sidebar.View())

		if sl.sidebarOnLeft {
			bodyContent = lipgloss.JoinHorizontal(lipgloss.Left, sidebarView, toggleCol, chatView)
		} else {
			bodyContent = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggleCol, sidebarView)
		}

	case sidebarCollapsed, sidebarCollapsedNarrow:
		switch {
		case p.leanMode:
			// Lean mode: no sidebar header, no fixed height
			bodyContent = styles.ChatStyle.
				Width(sl.innerWidth).
				Render(messagesView)
		case p.hideSidebar:
			// Sidebar hidden: chat fills the full height, no sidebar header.
			bodyContent = styles.ChatStyle.
				Height(sl.chatHeight).
				Width(sl.innerWidth).
				Render(messagesView)
		default:
			sidebarRendered := p.renderCollapsedSidebar(sl)
			chatView := styles.ChatStyle.
				Height(sl.chatHeight).
				Width(sl.innerWidth).
				Render(messagesView)
			if sl.bandAtBottom {
				bodyContent = lipgloss.JoinVertical(lipgloss.Top, chatView, sidebarRendered)
			} else {
				bodyContent = lipgloss.JoinVertical(lipgloss.Top, sidebarRendered, chatView)
			}
		}
	}

	appStyle := styles.AppStyle
	if !p.leanMode {
		appStyle = appStyle.Height(p.height)
	}
	return appStyle.Render(bodyContent)
}

// renderSidebarHandle renders the sidebar toggle/resize handle.
// The glyph points toward the edge the sidebar collapses to and flips when
// the sidebar sits on the left.
func (p *chatPage) renderSidebarHandle(height int) string {
	if height <= 0 {
		return ""
	}
	lines := make([]string, height)

	expandGlyph, collapseGlyph := "«", "»"
	if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
		expandGlyph, collapseGlyph = "»", "«"
	}

	glyph := collapseGlyph
	if p.presentationSidebarSettings().Collapsed {
		glyph = expandGlyph
	}
	lines[0] = styles.MutedStyle.Render(glyph)
	for i := 1; i < height; i++ {
		lines[i] = " "
	}

	return strings.Join(lines, "\n")
}

func (p *chatPage) SetSize(width, height int) tea.Cmd {
	p.width = width
	p.height = height
	if p.splitPresentation != nil {
		return p.applySplitPresentation()
	}

	var cmds []tea.Cmd

	// Compute layout once and use it for all sizing
	sl := p.computeSidebarLayout()
	p.appliedLayout = sl

	switch sl.mode {
	case sidebarVertical:
		p.sidebar.SetMode(sidebar.ModeVertical)
		p.sidebar.SetMirroredPadding(sl.sidebarOnLeft)
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth-toggleColumnWidth, sl.chatHeight),
			p.sidebar.SetPosition(styles.AppPadding+sl.sidebarStartX, 0),
			p.messages.SetPosition(styles.AppPadding+sl.chatStartX, 0),
		)
	case sidebarCollapsed, sidebarCollapsedNarrow:
		p.sidebar.SetMode(sidebar.ModeCollapsed)
		p.sidebar.SetMirroredPadding(false)
		messagesY := sl.sidebarHeight
		if sl.bandAtBottom {
			messagesY = 0
		}
		cmds = append(cmds,
			p.sidebar.SetSize(sl.sidebarWidth, sl.sidebarHeight),
			p.sidebar.SetPosition(styles.AppPadding, sl.bandY()),
			p.messages.SetPosition(styles.AppPadding, messagesY),
		)
	}

	cmds = append(cmds, p.messages.SetSize(sl.chatWidth, sl.chatHeight), p.reconcileSidebarLayout())

	return tea.Batch(cmds...)
}

// ResizeCacheStats reports transcript work on the owning event loop.
func (p *chatPage) ResizeCacheStats() (rebuilds, misses, renderedMessages uint64) {
	return p.messages.ResizeCacheStats()
}

// GetSize returns the current dimensions
func (p *chatPage) GetSize() (width, height int) {
	return p.width, p.height
}

// Bindings returns key bindings for the chat page
func (p *chatPage) Bindings() []key.Binding {
	return p.messages.Bindings()
}

// Help returns help information
func (p *chatPage) Help() help.KeyMap {
	return core.NewSimpleHelp(p.Bindings())
}

// CancelResponse uses the existing canonical cancellation path after root confirmation.
func CancelResponse(page Page) (tea.Cmd, bool) {
	p, ok := page.(*chatPage)
	if !ok || p.app == nil || p.app.CancelRun() == runtime.CancelNotActive {
		return nil, false
	}
	return p.finishCancelStream(true), true
}

func (p *chatPage) cancelStream(showCancelMessage bool) tea.Cmd {
	if p.app == nil || p.app.CancelRun() == runtime.CancelNotActive {
		return nil
	}
	return p.finishCancelStream(showCancelMessage)
}

func (p *chatPage) finishCancelStream(showCancelMessage bool) tea.Cmd {
	if p.msgCancel != nil {
		p.msgCancel()
		p.msgCancel = nil
	}

	p.streamCancelled = true
	p.lifecycle.Streams = nil
	p.setPendingResponse(false)
	// Send StreamCancelledMsg to all components to handle cleanup
	return tea.Batch(
		core.CmdHandler(msgtypes.StreamCancelledMsg{ShowMessage: showCancelMessage}),
		p.setWorking(false),
	)
}

func isBangCommand(content string) bool {
	return strings.HasPrefix(content, "!")
}

func (p *chatPage) parseImmediateCommand(content string) tea.Cmd {
	if p.commandParser == nil {
		return nil
	}
	return p.commandParser.Parse(content)
}

type resolvedSend struct {
	msg      msgtypes.SendMsg
	resolved app.ResolvedInput
}

type skillAdmissionMsg struct {
	operationID string
	err         error
}

// state they are processed immediately, steered into the ongoing stream, or
// queued until the current turn ends.
func (p *chatPage) handleSendMsg(msg msgtypes.SendMsg) (layout.Model, tea.Cmd) {
	// Handle "exit", "quit", and ":q" as special keywords to quit the session
	// immediately, equivalent to the /exit slash command.
	switch strings.TrimSpace(msg.Content) {
	case "exit", "quit", ":q":
		return p, core.CmdHandler(msgtypes.ExitSessionMsg{})
	}

	// Immediate UI slash commands (e.g. /exit, /compact) run even in read-only
	// mode. A BypassQueue message has already been resolved (e.g. an agent
	// command or fork-mode skill re-dispatching itself) and must skip parsing:
	// re-parsing would match the same command again and loop forever.
	if !msg.BypassQueue {
		if cmd := p.parseImmediateCommand(msg.Content); cmd != nil {
			return p, cmd
		}
	}

	// Everything below hands work to the model, which read-only sessions must
	// reject: normal input, bang commands, and resolved agent/skill commands
	// flagged BypassQueue.
	if p.app != nil && p.app.IsReadOnly() {
		return p, notification.WarningCmd("Session is read-only. No new messages can be sent.")
	}

	if isBangCommand(msg.Content) {
		cmd := p.processMessage(msg)
		return p, cmd
	}

	resolved, err := p.app.ResolveInputOnce(p.ctx(), msg.Content)
	if err != nil {
		return p, notification.ErrorCmd("Could not resolve input: " + err.Error())
	}
	classified := resolvedSend{msg: msg, resolved: resolved}
	if resolved.ForkSkill {
		cmd := p.processResolvedMessage(classified)
		return p, cmd
	}

	// If not working, process immediately
	if !p.working {
		cmd := p.processResolvedMessage(classified)
		return p, cmd
	}

	// Alt+Enter explicitly requests a separate end-of-turn follow-up. When the
	// agent is idle there is no active turn to follow, so process it normally.
	if msg.FollowUp && p.working && p.app != nil {
		cmd := p.followUpResolved(classified)
		return p, cmd
	}

	// While the agent is working, the configured send mode decides the
	// default: steer injects the message into the ongoing stream; queue
	// submits it to the session-owned FIFO for a later turn.
	if msg.Queue || msgtypes.ParseSendMode(string(p.sendMode)) == msgtypes.SendModeQueue {
		cmd := p.followUpResolved(classified)
		return p, cmd
	}

	cmd := p.steerResolved(classified)
	return p, cmd
}

// Command result messages report session admission only. Projection changes
// arrive separately through canonical accepted/promoted envelopes.
type (
	steerSentMsg      struct{}
	steerFailedMsg    struct{ err error }
	followUpSentMsg   struct{}
	followUpFailedMsg struct{ err error }
)

// steerMessage injects the message into the ongoing stream via the runtime's
// steer queue. Command resolution runs inside the command goroutine so slow
// skill/agent command expansion never blocks the UI. The transcript bubble is
// added when the runtime drains the message and emits its UserMessageEvent,
// which is the moment the agent actually sees it.
func (p *chatPage) steerResolved(input resolvedSend) tea.Cmd {
	ctx := p.ctx()
	msg, resolved := input.msg, input.resolved
	return func() tea.Msg {
		submission, err := p.app.SteerMessage(ctx, resolved.Content, msg.Attachments)
		if err != nil {
			slog.Warn("Failed to steer message", "error", err)
			return steerFailedMsg{err: err}
		}
		if submission.Disposition == runtime.SubmissionDispositionQueued {
			return followUpSentMsg{}
		}
		return steerSentMsg{}
	}
}

func (p *chatPage) followUpResolved(input resolvedSend) tea.Cmd {
	ctx := p.ctx()
	msg, resolved := input.msg, input.resolved
	return func() tea.Msg {
		if _, err := p.app.FollowUpMessage(ctx, resolved.Content, msg.Attachments); err != nil {
			slog.Warn("Failed to enqueue follow-up", "error", err)
			return followUpFailedMsg{err: err}
		}
		return followUpSentMsg{}
	}
}

func (p *chatPage) handleEditUserMessage(msg msgtypes.EditUserMessageMsg) (layout.Model, tea.Cmd) {
	if msg.SessionPosition < 0 || msg.MsgIndex < 0 {
		return p, nil
	}

	p.editing = true
	p.branchAtPosition = msg.SessionPosition

	// Extract any attachments from the original session message
	p.editAttachments = p.extractAttachmentsFromSession(msg.SessionPosition)

	// Start inline editing in the messages component.
	// Request focus switch to messages panel so the parent blurs the editor.
	editCmd := p.messages.StartInlineEdit(msg.MsgIndex, msg.SessionPosition, msg.OriginalContent)
	focusCmd := core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelMessages})

	return p, tea.Batch(editCmd, focusCmd)
}

// handleInlineEditCommitted handles the commit of an inline edit, triggering a branch.
func (p *chatPage) handleInlineEditCommitted(msg messages.InlineEditCommittedMsg) (layout.Model, tea.Cmd) {
	if !p.editing {
		return p, nil
	}

	p.editing = false
	branchPosition := p.branchAtPosition
	p.branchAtPosition = 0
	attachments := p.editAttachments
	p.editAttachments = nil

	var cancelCmd tea.Cmd
	if p.msgCancel != nil {
		cancelCmd = p.cancelStream(false)
	}

	parentID := ""
	if sess := p.app.Session(); sess != nil {
		parentID = sess.ID
	}

	branchCmd := core.CmdHandler(msgtypes.BranchFromEditMsg{
		ParentSessionID:  parentID,
		BranchAtPosition: branchPosition,
		Content:          msg.Content,
		Attachments:      attachments,
	})

	return p, tea.Batch(cancelCmd, branchCmd)
}

// handleInlineEditCancelled handles cancellation of an inline edit.
func (p *chatPage) handleInlineEditCancelled(msg messages.InlineEditCancelledMsg) (layout.Model, tea.Cmd) {
	p.editing = false
	p.branchAtPosition = 0
	p.editAttachments = nil

	if msg.WasInSelectionMode {
		// We were in keyboard selection mode before editing, stay in the messages panel.
		// The messages component already restored its selection state.
		return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelMessages})
	}
	// We weren't in selection mode, return focus to the editor.
	return p, core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor})
}

// extractAttachmentsFromSession extracts attachments from a session message at the given position.
// Legacy attachments are stored as text parts in MultiContent with format "Contents of <filename>: <dataURL>".
// New attachments are stored as Document parts.
func (p *chatPage) extractAttachmentsFromSession(position int) []msgtypes.Attachment {
	sess := p.app.Session()
	if sess == nil || position < 0 || position >= len(sess.Messages) {
		return nil
	}

	item := sess.Messages[position]
	if !item.IsMessage() || item.Message == nil {
		return nil
	}

	msg := item.Message.Message
	if len(msg.MultiContent) <= 1 {
		// No attachments - only the main text content or nothing
		return nil
	}

	var attachments []msgtypes.Attachment
	const legacyPrefix = "Contents of "

	// Skip the first part (main text content), look for attachment parts
	for i := 1; i < len(msg.MultiContent); i++ {
		part := msg.MultiContent[i]

		if part.Type == chat.MessagePartTypeDocument && part.Document != nil {
			content := part.Document.Source.InlineText
			data := part.Document.Source.InlineData
			mimeType := part.Document.MimeType

			if content != "" || len(data) > 0 {
				attachments = append(attachments, msgtypes.Attachment{
					Name:     part.Document.Name,
					Content:  content,
					MimeType: mimeType,
					Data:     data,
				})
			}
			continue
		}

		if part.Type != chat.MessagePartTypeText {
			continue
		}
		text := part.Text
		if !strings.HasPrefix(text, legacyPrefix) {
			continue
		}
		// Parse "Contents of <filename>: <dataURL>"
		rest := text[len(legacyPrefix):]
		before, after, ok := strings.Cut(rest, ": ")
		if !ok {
			continue
		}
		filename := before
		content := after
		if filename != "" && content != "" {
			attachments = append(attachments, msgtypes.Attachment{
				Name:    filename,
				Content: content,
			})
		}
	}

	return attachments
}

// restorePendingMessages recalls only inputs the session owner successfully withdrew.
// The canceled event, not this command result, updates the mailbox projection.
func (p *chatPage) restorePendingMessages() tea.Cmd {
	pending := append([]queuedMessage(nil), p.messageQueue...)
	ctx := p.ctx()
	return func() tea.Msg {
		var restored []string
		for _, msg := range pending {
			withdrawn, err := p.app.CancelPendingMessage(ctx, msg.turnID)
			if err == nil && withdrawn {
				restored = append(restored, msg.content)
			}
		}
		if len(restored) == 0 {
			return notification.InfoCmd("No pending messages")()
		}
		return tea.Batch(
			core.CmdHandler(msgtypes.RestorePendingMessagesMsg{Content: strings.Join(restored, "\n")}),
			core.CmdHandler(msgtypes.RequestFocusMsg{Target: msgtypes.PanelEditor}),
		)()
	}
}

// handleClearQueue cannot mutate the session mailbox behind its projection.
func (p *chatPage) handleClearQueue() (layout.Model, tea.Cmd) {
	if len(p.messageQueue) == 0 {
		return p, notification.InfoCmd("No messages queued")
	}
	return p, notification.WarningCmd("Accepted messages cannot be cleared")
}

// syncQueueToSidebar preserves canonical identity and full content for bounded rendering.
func (p *chatPage) syncQueueToSidebar() tea.Cmd {
	queuedPreviews := make([]sidebar.QueuedMessage, len(p.messageQueue))
	for i, queued := range p.messageQueue {
		queuedPreviews[i] = sidebar.QueuedMessage{ID: queued.turnID, Text: queued.content}
	}
	return p.sidebar.SetQueuedMessages(queuedPreviews)
}

// processMessage handles already-resolved bypass/bang compatibility input.
func (p *chatPage) processMessage(msg msgtypes.SendMsg) tea.Cmd {
	return p.processResolvedMessage(resolvedSend{msg: msg, resolved: app.ResolvedInput{Display: msg.Content, Content: msg.Content}})
}

// processResolvedMessage processes one classification without rediscovery.
func (p *chatPage) processResolvedMessage(input resolvedSend) tea.Cmd {
	msg, resolved := input.msg, input.resolved
	// Handle slash commands (e.g., /eval, /compact, /exit) BEFORE cancelling any ongoing stream.
	// These are UI commands that shouldn't interrupt the running agent.
	if !msg.BypassQueue {
		if cmd := p.parseImmediateCommand(msg.Content); cmd != nil {
			return cmd
		}
	}

	if isBangCommand(msg.Content) {
		p.app.RunBangCommand(p.ctx(), msg.Content[1:])
		return p.messages.ScrollToBottom()
	}

	if resolved.ForkSkill {
		operationID := app.NewSkillOperationID()
		p.ownedSkillOperation = operationID
		return func() tea.Msg {
			err := p.app.StartSkillForkOperation(p.ctx(), operationID, resolved.SkillName, resolved.SkillTask)
			return skillAdmissionMsg{operationID: operationID, err: err}
		}
	}

	if p.msgCancel != nil {
		p.msgCancel()
	}

	p.lifecycle.Streams = nil
	p.sidebar.ResetStreamTracking()

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(p.ctx())

	// Start working state immediately to show the user something is happening.
	// This provides visual feedback while the runtime loads tools and prepares the stream.
	spinnerCmd := p.setWorking(true)
	// Check if this is an agent command that needs resolution
	// If so, show a loading message with the command description
	var loadingCmd tea.Cmd
	if strings.HasPrefix(msg.Content, "/") {
		cmdName, _, _ := strings.Cut(msg.Content[1:], " ")
		if cmd, found := p.app.CurrentAgentCommands(ctx)[cmdName]; found {
			loadingCmd = p.messages.AddLoadingMessage(cmd.DisplayText())
		}
	}

	// Run the already-resolved input; no second discovery occurs here.
	go p.app.Run(ctx, p.msgCancel, resolved.Content, msg.Attachments)

	return tea.Batch(p.messages.ScrollToBottom(), spinnerCmd, loadingCmd)
}

// handleRetry re-runs the agent turn after an error, resuming the conversation
// from the current session state without adding a new user message.
func (p *chatPage) handleRetry() (layout.Model, tea.Cmd) {
	if p.app == nil || p.app.IsReadOnly() {
		return p, notification.WarningCmd("Session is read-only. No new messages can be sent.")
	}

	// Ignore retry requests while a turn is already in flight.
	if p.working {
		return p, nil
	}

	if p.msgCancel != nil {
		p.msgCancel()
	}

	p.lifecycle.Streams = nil
	p.sidebar.ResetStreamTracking()

	var ctx context.Context
	ctx, p.msgCancel = context.WithCancel(p.ctx())

	spinnerCmd := p.setWorking(true)
	p.app.Retry(ctx, p.msgCancel)

	return p, tea.Batch(p.messages.ScrollToBottom(), spinnerCmd)
}

// CompactSession requests session-owned compaction without mutating the page's
// run state until canonical compaction events arrive.
func (p *chatPage) CompactSession(additionalPrompt string) tea.Cmd {
	if p.app == nil {
		return notification.InfoCmd("Manual compaction is unavailable for this session")
	}
	ctx := p.ctx()
	return func() tea.Msg {
		if err := p.app.CompactSession(ctx, additionalPrompt); err != nil {
			if errors.Is(err, runtime.ErrUnsupported) {
				return notification.ShowMsg{Type: notification.TypeInfo, Text: "Manual compaction is unavailable for this session"}
			}
			return notification.ShowMsg{Type: notification.TypeError, Text: fmt.Sprintf("Compaction request failed: %v", err)}
		}
		return nil
	}
}

// SetSessionStarred updates the sidebar star indicator
func (p *chatPage) SetSessionStarred(starred bool) {
	p.sidebar.SetSessionStarred(starred)
}

func (p *chatPage) SetTitleRegenerating(regenerating bool) tea.Cmd {
	return p.sidebar.SetTitleRegenerating(regenerating)
}

// GetSidebarSettings returns the current sidebar display settings.
func (p *chatPage) GetSidebarSettings() SidebarSettings {
	return SidebarSettings{
		Collapsed:      p.sidebar.IsCollapsed(),
		PreferredWidth: p.sidebar.GetPreferredWidth(),
	}
}

// SetSidebarSettings applies sidebar display settings.
func (p *chatPage) SetSidebarSettings(settings SidebarSettings) {
	p.sidebar.SetCollapsed(settings.Collapsed)
	p.sidebar.SetPreferredWidth(settings.PreferredWidth)
}

// SetLayoutSettings applies layout customization and relayouts the page.
func (p *chatPage) SetLayoutSettings(settings msgtypes.LayoutSettings) tea.Cmd {
	p.layoutSettings = settings
	p.sidebar.SetSectionVisibility(sectionVisibility(settings))
	p.sidebar.SetSectionGap(settings.SectionSpacing.BlankLines())
	p.sidebar.SetAgentInfoMode(agentInfoMode(settings.SidebarInfoMode))
	p.sidebar.SetActiveAgentsOnly(settings.ActiveAgentsOnly)
	if p.width <= 0 || p.height <= 0 {
		return nil
	}
	return p.SetSize(p.width, p.height)
}

// SetSendMode sets the behavior of messages sent while the agent is working.
func (p *chatPage) SetSendMode(mode msgtypes.SendMode) {
	p.sendMode = mode
}

func (p *chatPage) SetInterruptMode(mode msgtypes.InterruptMode) {
	p.interruptMode = mode
}

func (p *chatPage) SetShowBanner(show bool) {
	p.hideBanner = !show
}

// SetRoutingID records the tab identity this page's routed UI timers are
// addressed to. See Page.SetRoutingID.
func (p *chatPage) SetRoutingID(id string) {
	p.routingID = id
	if owner, ok := p.messages.(interface{ SetImagePreviewSessionID(string) }); ok {
		owner.SetImagePreviewSessionID(id)
	}
}

// TakeRoutedTimers returns and clears the routed timer commands armed by the
// most recent Update. See Page.TakeRoutedTimers.
func (p *chatPage) TakeRoutedTimers() tea.Cmd {
	if len(p.pendingTimers) == 0 {
		return nil
	}
	cmd := tea.Batch(p.pendingTimers...)
	p.pendingTimers = nil
	return cmd
}

// scheduleTransferTimers arms the sidebar's one-shot presentation timers,
// addressed to this page: with a routing identity each expiry is wrapped in
// a messages.RoutedMsg so it lands on this page's tab even when another tab
// is active (or this one is hidden) by then; without one (standalone pages)
// the raw payload goes to the single active page. The commands are also
// recorded for TakeRoutedTimers so an update on a hidden page keeps its
// deadlines armed.
func (p *chatPage) scheduleTransferTimers(timers []sidebar.TransferTimer) tea.Cmd {
	if len(timers) == 0 {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(timers))
	for _, timer := range timers {
		cmds = append(cmds, p.routedTimerCmd(timer))
	}
	cmd := tea.Batch(cmds...)
	p.pendingTimers = append(p.pendingTimers, cmd)
	return cmd
}

// routedTimerCmd schedules one timer, wrapping its payload in the page's
// routing envelope. The routing ID is captured by value: the command runs on
// a background goroutine and must not read page state later.
func (p *chatPage) routedTimerCmd(timer sidebar.TransferTimer) tea.Cmd {
	routingID := p.routingID
	if routingID == "" {
		return timer.Cmd()
	}
	return tea.Tick(timer.Duration, func(time.Time) tea.Msg {
		return msgtypes.RoutedMsg{SessionID: routingID, Inner: timer.Msg}
	})
}

func (p *chatPage) PointerTargetsMessages(x, y int) bool {
	if g := p.splitPresentation; g != nil {
		return !p.presentationHidden && g.Transcript.contains(x, y)
	}
	sl := p.computeSidebarLayout()
	return !sl.isInBand(y) && (sl.mode != sidebarVertical || p.presentationSidebarSettings().Collapsed || !sl.isInSidebar(x-styles.AppPadding))
}

// handleSidebarClickType checks what was clicked in the sidebar area.
// Returns the click type and, for ClickAgent, the agent name.
func (p *chatPage) handleSidebarClickType(x, y int) (sidebar.ClickResult, string) {
	if g := p.splitPresentation; g != nil {
		if p.sidebarInteractive() && g.Shell.Sidebar.contains(x, y) {
			return p.sidebar.HandleClickType(x-g.Shell.Sidebar.X, y-g.Shell.Sidebar.Y)
		}
		return sidebar.ClickNone, ""
	}
	adjustedX := x - styles.AppPadding
	sl := p.computeSidebarLayout()

	switch sl.mode {
	case sidebarCollapsedNarrow, sidebarCollapsed:
		return p.sidebar.HandleClickType(adjustedX, sl.bandContentY(y))
	case sidebarVertical:
		if sl.isInSidebar(adjustedX) {
			return p.sidebar.HandleClickType(adjustedX-sl.sidebarStartX, y)
		}
	}

	return sidebar.ClickNone, ""
}

// routeMouseEvent routes mouse events to the appropriate component based on coordinates.
func (p *chatPage) routeMouseEvent(msg tea.Msg, _ int) tea.Cmd {
	if p.splitPresentation != nil {
		return p.routeSplitMouseEvent(msg)
	}
	sl := p.computeSidebarLayout()

	var x, y int
	switch m := msg.(type) {
	case tea.MouseClickMsg:
		x, y = m.X, m.Y
	case tea.MouseMotionMsg:
		x, y = m.X, m.Y
	case tea.MouseReleaseMsg:
		x, y = m.X, m.Y
	}
	adjustedX := x - styles.AppPadding
	inSidebar := sl.mode == sidebarVertical && !p.presentationSidebarSettings().Collapsed && sl.isInSidebar(adjustedX)
	inBand := !p.hideSidebar && !p.leanMode && sl.isInBand(y) && adjustedX >= 0 && adjustedX < sl.innerWidth
	if inSidebar || inBand {
		p.messages.ClearReferenceHover()
		model, cmd := p.sidebar.Update(msg)
		p.sidebar = model.(sidebar.Model)
		return cmd
	}

	// Motion left the sidebar: preserve the finite hover-exit command.
	var hoverCmd tea.Cmd
	if _, ok := msg.(tea.MouseMotionMsg); ok {
		hoverCmd = p.sidebar.ClearSubagentHover()
	}

	model, cmd := p.messages.Update(msg)
	p.messages = model.(messages.Model)
	return tea.Batch(hoverCmd, cmd)
}

// IsWorking returns whether the agent is currently working
func (p *chatPage) IsWorking() bool {
	return p.working
}

// IsInlineEditing returns true if a past user message is being edited inline.
func (p *chatPage) IsInlineEditing() bool {
	return p.messages.IsInlineEditing()
}

// IsSelecting returns true while a text-selection drag is active in the
// messages panel.
func (p *chatPage) IsSelecting() bool {
	return p.messages.IsSelecting()
}

// QueueLength returns the number of queued messages
func (p *chatPage) QueueLength() int {
	return len(p.messageQueue)
}

// FocusMessages gives focus to the messages panel
func (p *chatPage) FocusMessages() tea.Cmd {
	return p.messages.Focus()
}

// FocusMessageAt gives focus and selects the message at the given screen coordinates.
func (p *chatPage) FocusMessageAt(x, y int) tea.Cmd {
	return p.messages.FocusAt(x, y)
}

// BlurMessages removes focus from the messages panel
func (p *chatPage) BlurMessages() {
	p.messages.Blur()
}

// ScrollToBottom scrolls the messages viewport to the bottom if auto-scroll is active.
func (p *chatPage) ScrollToBottom() tea.Cmd {
	return p.messages.ScrollToBottom()
}

// IsTitleEditing reports whether the sidebar title input is active.
func (p *chatPage) IsTitleEditing() bool {
	return p.sidebarInteractive() && p.sidebar.IsEditingTitle()
}

// CancelImagePreviewClick releases a pending image press when a modal takes input.
func (p *chatPage) CancelImagePreviewClick() {
	if owner, ok := p.messages.(interface{ CancelImagePreviewClick() }); ok {
		owner.CancelImagePreviewClick()
	}
}
