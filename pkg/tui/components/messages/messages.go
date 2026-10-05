package messages

import (
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/internal/termfeatures"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/tui/widgets/help"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	"github.com/docker/docker-agent/pkg/tui/widgets/textarea"
)

// ToggleHideToolResultsMsg triggers hiding/showing tool results
type ToggleHideToolResultsMsg struct{}

// SessionState is the session-state surface the message list depends on:
// read access for rendering, plus the two mutations the component performs.
// *service.SessionState satisfies it; embedders outside the full TUI can
// provide their own implementation.
type SessionState interface {
	service.SessionStateReader
	SetPreviousMessage(msg *types.Message)
	ToggleHideToolResults()
}

var (
	_ SessionState = (*service.SessionState)(nil)
	_ SessionState = (*service.EmbeddedSessionState)(nil)
)

// scrollToBottomMsg requests the message list scroll to the bottom. It is
// returned by commands (e.g. after appending a message) instead of mutating
// scroll state directly from the command goroutine: bubbletea runs command
// closures on their own goroutines, so writing scrollOffset / userHasScrolled
// / bottomSlack there races with View() / updateScrollState() on the event
// loop. Handling it as a message keeps all scroll-state mutation on the
// single Update/View goroutine.
type scrollToBottomMsg struct{}

type toggleableView interface {
	IsToggleLine(lineIdx int) bool
	Toggle()
}

// Model represents a chat message list component
type Model interface {
	layout.Model
	layout.Sizeable
	layout.Focusable
	layout.Help
	layout.Positionable

	AddUserMessage(content string) tea.Cmd
	AddLoadingMessage(description string) tea.Cmd
	ReplaceLoadingWithUser(content string, sessionPos int) tea.Cmd
	AddInputMessage(msg *session.Message, sessionPos int) tea.Cmd
	AddErrorMessage(content string) tea.Cmd
	AddAssistantMessage(sender, label string) tea.Cmd
	AddCancelledMessage() tea.Cmd
	AddWelcomeMessage(content string) tea.Cmd
	AddOrUpdateToolCall(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status types.ToolStatus) tea.Cmd
	AppendToolOutput(msg *runtime.ToolCallOutputEvent) tea.Cmd
	AddToolResult(msg *runtime.ToolCallResponseEvent, status types.ToolStatus) tea.Cmd
	AppendToLastMessage(agentName, content string) tea.Cmd
	// AppendAssistantMedia attaches generated media to the agent's current
	// assistant message (or starts a media-only one), so it renders in the
	// same assistant turn as the streamed text.
	AppendAssistantMedia(agentName string, media []types.AssistantMedia) tea.Cmd
	// UpdateAssistantMedia replaces previously attached media items —
	// wherever they sit in the list — with the given resolved items, matched
	// by types.AssistantMedia.ID. Items with unknown or zero IDs are
	// ignored, so a stale asynchronous result is harmless.
	UpdateAssistantMedia(media []types.AssistantMedia) tea.Cmd
	AppendReasoning(agentName, content string) tea.Cmd
	AddShellOutputMessage(content string) tea.Cmd
	// AddAgentReturn appends the UI-only "child returned control to parent"
	// delegation transition. It is never persisted, so it does not reappear
	// when the session is reloaded.
	AddAgentReturn(fromAgent, toAgent string) tea.Cmd
	// LoadFromSession rebuilds the list from a persisted session.
	// generatedMedia carries the restored generated-media items to attach,
	// keyed by the owning message's index in sess.Messages; nil when the
	// caller cannot resolve generated media.
	LoadFromSession(sess *session.Session, generatedMedia map[int][]types.AssistantMedia) tea.Cmd
	LoadedItemCount() int
	// RemovePendingSessionPosition shifts restored message positions after durable withdrawal.
	RemovePendingSessionPosition(position int)
	// FinalizeStreamedAssistant settles the streamed tail of an assistant
	// message against its committed content (a MessageAddedEvent):
	// alreadyShown drops the trailing streamed bubbles (the message came with
	// the transcript snapshot and is on screen), otherwise the tail is
	// finalized in place with the canonical content — creating the bubbles if
	// streaming deltas were missed entirely. Never touches scroll state.
	FinalizeStreamedAssistant(agentName, content, reasoning string, alreadyShown bool) tea.Cmd
	CommitAssistant(agentName string, toolCallIDs []string) tea.Cmd
	CompleteAssistant(event *runtime.MessageAddedEvent) tea.Cmd
	// InvalidateRenderCaches drops all cached message renders so the next View
	// re-renders with current styling (e.g. after the agent color registry
	// changes on a TeamInfoEvent that arrived after a session restore).
	InvalidateRenderCaches()
	ResizeCacheStats() (rebuilds, misses, renderedMessages uint64)

	// StopAnimations unregisters every view from the animation coordinator.
	// Call it when the list is discarded or its host view goes away, so
	// abandoned spinners do not keep the tick stream alive.
	StopAnimations()

	RemoveSpinner()
	ScrollToBottom() tea.Cmd
	// FinalizeStream materializes any offscreen active tail before stream
	// completion/cancellation makes message content externally observable.
	FinalizeStream() tea.Cmd
	AdjustBottomSlack(delta int)
	// VisualGeneration increments only when Update changes rendered output.
	VisualGeneration() uint64

	// MessageTypeCount returns how many messages currently in the list have
	// the given type. Read-only introspection for callers (e.g. tests) that
	// need to observe real list state rather than trust a call was made.
	MessageTypeCount(t types.MessageType) int

	// IsScrollbarDragging returns true when the scrollbar thumb is being dragged.
	IsScrollbarDragging() bool

	// IsSelecting returns true while a text-selection drag is in progress.
	IsSelecting() bool

	// IsMouseOnScrollbar returns true when the given screen coordinates are on the scrollbar.
	IsMouseOnScrollbar(x, y int) bool

	// Inline editing methods
	StartInlineEdit(msgIndex, sessionPosition int, content string) tea.Cmd
	CancelInlineEdit() tea.Cmd
	IsInlineEditing() bool

	// FocusAt gives focus and selects the message at the given screen coordinates.
	// Falls back to the default Focus behavior if no message is found at that position.
	FocusAt(x, y int) tea.Cmd

	// SubagentNodeAt returns the subagent node id referenced by the subagent
	// tool message at the given screen coordinates, if any.
	SubagentNodeAt(x, y int) (subagent.NodeID, bool)
	InputReferenceAt(x, y int) (lifecycle.InputReference, bool)
	RefreshInputReferences()
	ClearReferenceHover() tea.Cmd
	CancelReferenceHover()
	SetReferencePresentationActive(bool)
	RenderedContentHeight() int
}

type visualSelectionKey struct {
	active              bool
	startLine, startCol int
	endLine, endCol     int
}

type transcriptFrameKey struct {
	scrollbarDragging                   bool
	contentGeneration, segmentsRevision uint64
	width, height, offset, total, slack int
	selection                           visualSelectionKey
	hoveredURL                          hoveredURL
	hasHoveredURL                       bool
	referenceHoverGeneration            uint64
	copiedFlash                         copiedFlash
	hasCopiedFlash                      bool
}

// renderedItem represents a cached rendered message with position information
type renderedItem struct {
	lines    []string // Pre-split rendered lines (shared with the joined renderedLines slice)
	segments *message.AssistantSegments
	height   int // Height in lines
}

type activeTranscriptSegments struct {
	index  int
	start  int
	header []string
	stable []string
	tail   []string
}

func (s *activeTranscriptSegments) height() int {
	if s == nil {
		return 0
	}
	return len(s.header) + len(s.stable) + len(s.tail)
}

func (s *activeTranscriptSegments) line(local int) string {
	if local < len(s.header) {
		return s.header[local]
	}
	local -= len(s.header)
	if local < len(s.stable) {
		return s.stable[local]
	}
	return s.tail[local-len(s.stable)]
}

// renderedItemRange indexes the single retained transcript line buffer. Historical
// entries retain no second line payload or markdown renderer state.
type renderedItemRange struct {
	start, height int
}

type renderedItemIndex map[int]renderedItemRange

func (r renderedItemIndex) Len() int         { return len(r) }
func (r renderedItemIndex) Clear()           { clear(r) }
func (r renderedItemIndex) Delete(index int) { delete(r, index) }

// blockIDCounter generates unique IDs for reasoning blocks.
var blockIDCounter atomic.Uint64

func nextBlockID() string {
	id := blockIDCounter.Add(1)
	return "block-" + strconv.FormatUint(id, 10)
}

// model implements Model
type model struct {
	imageClick                    *imageClick
	imageSessionID                string
	keyboardEnhancementsSupported bool
	ar                            *animation.Runtime
	messages                      []*types.Message
	views                         []layout.Model
	width                         int // Full width including scrollbar space
	height                        int

	// Height tracking system fields
	scrollOffset       int // Current scroll position in lines
	bottomSlackElapsed time.Duration
	bottomSlack        int                       // Extra blank lines added after content shrinks
	slackAnimationSub  animation.Subscription    // Subscription to animation ticks while slack > 0
	renderedLines      []string                  // Cached flattened content excluding a segmented active suffix
	activeSegments     *activeTranscriptSegments // Segmented final assistant item while visibly streaming
	renderedItems      renderedItemIndex         // Metadata into renderedLines, not a second payload cache
	urlSpans           *urlSpanCache             // Cached URL spans per rendered line
	lineOffsets        []int                     // Prefix-sum: lineOffsets[i] = starting global line of view i
	totalHeight        int                       // Total height of all content in lines
	replay             *sessionReplay
	renderDirty        bool // True when rendered content needs rebuild

	themeGeneration    uint64
	transcriptRebuilds uint64
	itemMisses         uint64
	renderedMessages   uint64
	visualGeneration   uint64
	contentGeneration  uint64
	segmentsRevision   uint64
	lastFrameKey       transcriptFrameKey
	lastFrameOutput    string

	selection selectionState

	sessionState         SessionState
	subagents            *subagentindex.Index
	inputParentSessionID string
	scrollview           *scrollview.Model

	xPos, yPos int

	// User scroll state
	userHasScrolled   bool // True when user manually scrolls away from bottom
	deferredTailIndex int
	deferredTail      []string

	// Message selection state
	selectedMessageIndex int // Index of selected message (-1 = no selection)
	// loadedItemCount is the length of the item snapshot last rendered by
	// LoadFromSession (0 when never loaded), taken from the same copy as the
	// rendered items. loadedMessageCount is how many view messages that
	// render produced: reconciliation never reaches below it, so snapshot
	// bubbles are untouchable even when adjacent to streamed ones.
	loadedItemCount       int
	loadedMessageCount    int
	committedMessageCount int
	focused               bool // Whether the messages component is focused

	// Inline editing state
	inlineEditMsgIndex      int            // Index of message being edited (-1 = not editing)
	inlineEditSessionPos    int            // Session position for branching
	inlineEditTextarea      textarea.Model // Textarea for inline editing
	inlineEditOriginal      string         // Original content (for cancel)
	inlineEditPrevSelection int            // Previous selection index before entering inline edit (-1 = was not in selection mode)

	// Hover state for showing action labels (copy, edit) on messages
	hoveredMessageIndex int // Index of message under mouse (-1 = none)

	// Transient "copied" confirmation over the last clicked copy label
	copiedFlash    *copiedFlash
	copiedFlashSeq int

	// Hovered URL for underline-on-hover effect (nil = no URL hovered)
	hoveredURL                  *hoveredURL
	referenceHoverAnimation     animation.Subscription
	referenceHoverTarget        referenceHoverKey
	referenceHoverValues        map[referenceHoverKey]referenceHoverValue
	referenceHoverGeneration    uint64
	referencePresentationHidden bool
}

// New creates a new message list component
func New(ar *animation.Runtime, sessionState SessionState, indexes ...*subagentindex.Index) Model {
	return newModel(ar, 120, 24, sessionState, indexes...)
}

// NewScrollableView creates a simple scrollable view for displaying messages in dialogs
// This is a lightweight version that doesn't require app or session state management
func NewScrollableView(ar *animation.Runtime, width, height int, sessionState SessionState) Model {
	return newModel(ar, width, height, sessionState)
}

func newModel(ar *animation.Runtime, width, height int, sessionState SessionState, indexes ...*subagentindex.Index) *model {
	sv := scrollview.New(
		scrollview.WithReserveScrollbarSpace(true),
	)
	sv.SetSize(width, height)
	if ar == nil {
		panic("messages: nil animation runtime")
	}
	var index *subagentindex.Index
	if len(indexes) > 0 {
		index = indexes[0]
	}
	return &model{
		ar:                      ar,
		themeGeneration:         styles.ThemeGeneration(),
		slackAnimationSub:       ar.Subscribe(),
		referenceHoverAnimation: ar.Subscribe(),
		width:                   width,
		height:                  height,
		renderedItems:           make(renderedItemIndex),
		urlSpans:                newURLSpanCache(),
		sessionState:            sessionState,
		subagents:               index,
		scrollview:              sv,
		selectedMessageIndex:    -1,
		inlineEditMsgIndex:      -1,
		hoveredMessageIndex:     -1,
		renderDirty:             true,
	}
}

// Init initializes the component
func (m *model) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, view := range m.views {
		if cmd := view.Init(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// Update handles messages and updates the component state
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if _, wheel := msg.(tea.MouseWheelMsg); wheel {
		m.cancelImageClick()
	}
	var cmds []tea.Cmd
	var animatedBeforeTick []int
	if _, ok := msg.(animation.TickMsg); ok {
		for i := range m.messages {
			if m.itemNeedsTick(i) {
				animatedBeforeTick = append(animatedBeforeTick, i)
			}
		}
	}

	switch msg := msg.(type) {
	case messages.StreamCancelledMsg:
		finalizeCmd := m.FinalizeStream()
		m.removeSpinner()
		m.removePendingToolCallMessages()
		m.stopReasoningBlockAnimations()
		return m, finalizeCmd

	case tea.WindowSizeMsg:
		cmds = append(cmds, m.SetSize(msg.Width, msg.Height))

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseMotionMsg:
		return m.handleMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.handleMouseRelease(msg)

	case messages.WheelCoalescedMsg:
		m.cancelImageClick()
		cmd := m.scrollByWheel(msg.Delta)
		return m, cmd

	case AutoScrollTickMsg:
		if m.selection.mouseButtonDown && m.selection.active {
			cmd := m.autoScroll()
			return m, cmd
		}
		return m, nil

	case DebouncedCopyMsg:
		cmd := m.handleDebouncedCopy(msg)
		return m, cmd

	case copiedFlashExpiredMsg:
		m.handleCopiedFlashExpired(msg)
		return m, nil

	case scrollToBottomMsg:
		if !m.userHasScrolled {
			cmd := m.scrollToBottom()
			return m, cmd
		}
		return m, nil

	case tea.KeyboardEnhancementsMsg:
		m.keyboardEnhancementsSupported = msg.Flags != 0 || termfeatures.SupportsModifiedEnter(os.Getenv)
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.KeyMap.InsertNewline.SetKeys(core.EditorNewlineKeys(m.keyboardEnhancementsSupported)...)
		}
		return m, nil

	case editfile.ToggleDiffViewMsg:
		m.invalidateAllItems()
		return m, nil

	case messages.SessionToggleChangedMsg:
		m.invalidateAllItems()
		return m, nil

	case ToggleHideToolResultsMsg:
		m.sessionState.ToggleHideToolResults()
		m.invalidateAllItems()
		return m, nil

	case messages.ThemeChangedMsg:
		m.themeGeneration = styles.ThemeGeneration()
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.SetStyles(inlineEditStyles())
		}
		// Theme changed - invalidate all render caches
		m.invalidateAllItems()
		editfile.InvalidateCaches()
		for i, view := range m.views {
			updatedView, cmd := view.Update(msg)
			m.views[i] = updatedView
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)

	case animation.TickMsg:
		// Child owners mark the shared tick dirty only at visible frame
		// boundaries. Invalidate after fanout below.

	case tea.PasteMsg:
		// Insert paste content into the inline edit textarea
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.InsertString(msg.Content)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	}

	// Forward updates to all message views
	for i, view := range m.views {
		updatedView, cmd := view.Update(msg)
		m.views[i] = updatedView
		if cmd != nil {
			cmds = append(cmds, cmd)
			// Child state changed (e.g., spinner tick), invalidate render cache
			m.invalidateItem(i)
		}
	}

	// On animation ticks, decay leftover bottom slack and keep the slack
	// subscription in sync so empty lines don't persist after thinking text
	// fades out. Must run after children update so reasoning blocks have
	// applied their fade state, and before tui.go's HasActive() check so the
	// subscription is registered when the next tick is scheduled.
	if tick, ok := msg.(animation.TickMsg); ok {
		cmds = append(cmds, m.handleAnimationTick(tick))
		// Tick dirtiness is program-wide. Do not rebuild the entire transcript
		// merely because the root/sidebar spinner advanced; only message-owned
		// animated content can change this component's lines.
		if tick.Dirty() {
			// Retain pending tool frames between deltas, but evict animated
			// ranges on frame changes, including a block's terminal fade tick.
			for _, i := range animatedBeforeTick {
				m.invalidateItem(i)
			}
		}
		m.tickReferenceHover(tick)
	}

	return m, tea.Batch(cmds...)
}

func (m *model) handleMouseClick(msg tea.MouseClickMsg) (model layout.Model, cmd tea.Cmd) {
	m.cancelImageClick()
	m.CancelReferenceHover()
	var materializeCmd tea.Cmd
	defer func() { cmd = tea.Batch(materializeCmd, cmd) }()
	// Scrollbar hit-testing and thumb geometry must use the exact tail height.
	// Checking the column first avoids materializing for ordinary transcript
	// clicks that cannot reach the stale final item.
	if msg.X == m.scrollview.ScrollbarX() && msg.Y >= m.yPos && msg.Y < m.yPos+m.height {
		materializeCmd = m.materializeDeferredTailForInteraction()
	}
	if m.isMouseOnScrollbar(msg.X, msg.Y) {
		return m.handleScrollviewUpdate(msg)
	}

	if msg.Button != tea.MouseLeft {
		return m, nil
	}

	line, col := m.mouseToLineCol(msg.X, msg.Y)

	msgIdx, localLine, interactionCmd := m.globalLineToMessageLine(line)
	materializeCmd = tea.Batch(materializeCmd, interactionCmd)
	if msgIdx >= 0 {
		// Check for toggleable blocks (e.g. reasoning block, collapsed long messages)
		if t, ok := m.views[msgIdx].(toggleableView); ok {
			var toggle bool
			if precise, ok := t.(interface{ IsToggleAt(line, col int) bool }); ok {
				toggle = precise.IsToggleAt(localLine, col)
			} else {
				toggle = t.IsToggleLine(localLine)
			}
			if toggle {
				if nested, ok := t.(interface{ ToggleAt(line, col int) }); ok {
					nested.ToggleAt(localLine, col)
				} else {
					t.Toggle()
				}
				m.bottomSlack = 0
				m.invalidateItem(msgIdx)
				return m, nil
			}
		}

		if m.isEditLabelClick(msgIdx, localLine, col) {
			msg := m.messages[msgIdx]
			return m, core.CmdHandler(messages.EditUserMessageMsg{
				MsgIndex:        msgIdx,
				SessionPosition: *msg.SessionPosition,
				OriginalContent: msg.Content,
			})
		}

		if content, ok := m.codeBlockAt(msgIdx, localLine, col); ok {
			return m, tea.Batch(copyTextToClipboardSilent(content), m.flashCopiedLabel(msgIdx, localLine, true))
		}

		if m.isCopyLabelClick(msgIdx, localLine, col) {
			cmd := m.copyMessageToClipboard(msgIdx)
			if cmd == nil {
				return m, nil
			}
			return m, tea.Batch(cmd, m.flashCopiedLabel(msgIdx, localLine, false))
		}

		if m.isRetryLabelClick(msgIdx, localLine, col) {
			return m, core.CmdHandler(messages.RetryMsg{})
		}
	}

	if url := m.urlAt(line, col); url != "" {
		return m, core.CmdHandler(messages.OpenURLMsg{URL: url})
	}

	if m.beginImageClick(msg) {
		return m, nil
	}

	clickCount := m.selection.detectClickType(line, col)

	switch clickCount {
	case 3: // Triple-click: select line, drag extends it, copy on release/debounce
		if m.selectLineAt(line) {
			m.selection.mouseButtonDown = true
			m.selection.mouseY = msg.Y
			m.selection.anchorTo()
			cmd := m.scheduleDebouncedCopy()
			return m, cmd
		}
		// Blank line: fall back to a plain drag selection.
		m.selection.start(line, col)
		m.selection.mouseY = msg.Y
		return m, nil
	case 2: // Double-click: select word, drag extends it, copy on release/debounce
		if m.selectWordAt(line, col) {
			// Keep tracking the button so a double-click drag extends the
			// selection word-anchored instead of being ignored.
			m.selection.mouseButtonDown = true
			m.selection.mouseY = msg.Y
			m.selection.anchorTo()
			cmd := m.scheduleDebouncedCopy()
			return m, cmd
		}
		// Blank line: fall back to a plain drag selection.
		m.selection.start(line, col)
		m.selection.mouseY = msg.Y
		return m, nil
	default: // Single click: start drag selection
		m.selection.start(line, col)
		m.selection.mouseY = msg.Y
		return m, nil
	}
}

// globalLineToMessageLine maps a global line index to (message index, local line within message).
// Returns (-1, -1) if the line doesn't correspond to any message.
func (m *model) globalLineToMessageLine(globalLine int) (msgIdx, localLine int, cmd tea.Cmd) {
	cmd = m.materializeDeferredTailForRange(globalLine, globalLine+1)
	msgIdx, localLine = m.globalLineToMessageLineCached(globalLine)
	return msgIdx, localLine, cmd
}

// globalLineToMessageLineCached maps against the currently owned transcript
// geometry without reconciling a deferred streaming tail. Pointer hover is a
// visual-only operation: it may restyle an already materialized line, but it
// must not make offscreen content become geometry.
func (m *model) globalLineToMessageLineCached(globalLine int) (msgIdx, localLine int) {
	m.ensureAllItemsRendered()

	if len(m.lineOffsets) == 0 || globalLine < 0 || globalLine >= m.totalHeight {
		return -1, -1
	}

	// Binary search: find the last view whose offset <= globalLine
	i := sort.Search(len(m.lineOffsets), func(i int) bool {
		return m.lineOffsets[i] > globalLine
	}) - 1

	if i < 0 || i >= len(m.views) {
		return -1, -1
	}

	start := m.lineOffsets[i]
	end := m.totalHeight
	if i+1 < len(m.lineOffsets) {
		end = m.lineOffsets[i+1]
	}
	if m.needsSeparator(i) && end > start && end <= len(m.renderedLines) && m.renderedLines[end-1] == "" {
		end--
	}
	local := globalLine - start
	if local >= 0 && globalLine < end {
		return i, local
	}

	// globalLine falls in a separator gap between messages
	return -1, -1
}

func (m *model) handleMouseMotion(msg tea.MouseMotionMsg) (layout.Model, tea.Cmd) {
	if m.imageClick != nil && (msg.X != m.imageClick.x || msg.Y != m.imageClick.y) {
		m.cancelImageClick()
	}
	if m.scrollview.IsDragging() {
		m.CancelReferenceHover()
		materializeCmd := m.materializeDeferredTailForInteraction()
		model, cmd := m.handleScrollviewUpdate(msg)
		return model, tea.Batch(materializeCmd, cmd)
	}

	if m.selection.mouseButtonDown && m.selection.active {
		m.CancelReferenceHover()
		line, col := m.mouseToLineCol(msg.X, msg.Y)
		// Any real movement turns a multi-click into a drag: the copy now
		// belongs to the release handler, not the debounced word copy.
		if line != m.selection.lastClickLine || col != m.selection.lastClickCol {
			m.selection.pendingCopyID++
		}
		m.selection.update(line, col)
		m.selection.mouseY = msg.Y
		cmd := m.autoScroll()
		return m, cmd
	}

	// Track hovered message for showing the action labels (copy, edit)
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	newHovered := -1
	if msgIdx, _ := m.globalLineToMessageLineCached(line); msgIdx >= 0 && msgIdx < len(m.messages) {
		switch m.messages[msgIdx].Type {
		case types.MessageTypeAssistant, types.MessageTypeUser:
			newHovered = msgIdx
		}
	}
	if newHovered != m.hoveredMessageIndex {
		oldHovered := m.hoveredMessageIndex
		m.hoveredMessageIndex = newHovered
		m.refreshHoverItems(oldHovered, newHovered)
		m.visualGeneration++
	}

	// Track hovered URL for underline effect
	if msg.X < m.xPos || msg.X >= m.xPos+m.contentWidth() || msg.Y < m.yPos || msg.Y >= m.yPos+m.height {
		m.ClearReferenceHover()
	} else {
		m.updateHoveredURL(line, col)
	}

	return m, nil
}

func (m *model) handleMouseRelease(msg tea.MouseReleaseMsg) (layout.Model, tea.Cmd) {
	if cmd, handled := m.releaseImageClick(msg); handled {
		return m, cmd
	}
	if m.scrollview.IsDragging() {
		// An owned release is consumed even when no command is produced.
		// Never turn a scrollbar gesture into a text copy or URL click.
		return m.handleScrollviewUpdate(msg)
	}

	if msg.Button == tea.MouseLeft && m.selection.mouseButtonDown {
		if m.selection.active {
			line, col := m.mouseToLineCol(msg.X, msg.Y)

			// If the mouse didn't move since the press, this wasn't a drag.
			if line == m.selection.lastClickLine && col == m.selection.lastClickCol {
				if m.selection.hasRange() {
					// Word/line selection from a multi-click: keep it, the
					// debounced copy fires.
					m.selection.end()
					return m, nil
				}
				// Plain click — open URL if any
				m.selection.clear()
				if url := m.urlAt(line, col); url != "" {
					return m, core.CmdHandler(messages.OpenURLMsg{URL: url})
				}
				return m, nil
			}

			m.selection.update(line, col)
			m.selection.end()
			m.selection.pendingCopyID++ // The drag copy supersedes any pending word copy
			// A completed drag is not part of a multi-click sequence: the next
			// press must start a fresh selection, not a word/line selection.
			m.selection.resetClickTracking()
			cmd := m.copySelectionToClipboard()
			return m, cmd
		}
		m.selection.end()
	}
	return m, nil
}

func (m *model) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	m.cancelImageClick()
	// Handle inline editing keys first
	if m.inlineEditMsgIndex >= 0 {
		// Check for newline insertion using key.Matches against the textarea's InsertNewline binding
		// This properly handles shift+enter and ctrl+j based on the configured keymap
		if key.Matches(msg, m.inlineEditTextarea.KeyMap.InsertNewline) {
			// Forward to textarea for newline insertion
			var cmd tea.Cmd
			m.inlineEditTextarea, cmd = m.inlineEditTextarea.Update(msg)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
			return m, cmd
		}

		// The configured send key commits the edit (Enter by default), mirroring
		// the composer so a remapped editor_send works here too.
		if key.Matches(msg, core.GetKeys().EditorSend) {
			cmd := m.commitInlineEdit()
			return m, cmd
		}

		switch msg.Key().Code {
		case tea.KeyEscape:
			// Esc cancels the edit
			cmd := m.CancelInlineEdit()
			return m, cmd
		default:
			// Forward all other keys to the textarea
			var cmd tea.Cmd
			m.inlineEditTextarea, cmd = m.inlineEditTextarea.Update(msg)
			m.invalidateItem(m.inlineEditMsgIndex)
			m.renderDirty = true
			return m, cmd
		}
	}

	switch msg.String() {
	case "esc":
		m.clearSelection()
		return m, nil
	case "up", "k":
		if m.focused {
			m.selectPreviousMessage()
			return m, nil
		} else {
			m.scrollUp()
		}
		return m, nil
	case "down", "j":
		if m.focused {
			m.selectNextMessage()
			return m, nil
		} else {
			cmd := m.scrollDown()
			return m, cmd
		}
	case "c":
		if m.focused && m.selectedMessageIndex >= 0 {
			cmd := m.copySelectedMessageToClipboard()
			return m, cmd
		}
		return m, nil
	case "enter", "space":
		if m.focused && m.selectedMessageIndex >= 0 {
			if view, ok := m.views[m.selectedMessageIndex].(toggleableView); ok {
				view.Toggle()
				m.bottomSlack = 0
				m.invalidateItem(m.selectedMessageIndex)
			}
		}
		return m, nil
	case "e":
		if m.focused && m.selectedMessageIndex >= 0 {
			msg := m.messages[m.selectedMessageIndex]
			if msg.Type == types.MessageTypeUser && msg.SessionPosition != nil {
				return m, func() tea.Msg {
					return messages.EditUserMessageMsg{
						MsgIndex:        m.selectedMessageIndex,
						SessionPosition: *msg.SessionPosition,
						OriginalContent: msg.Content,
					}
				}
			}
		}
		return m, nil
	case "pgup":
		m.scrollPageUp()
		return m, nil
	case "pgdown":
		cmd := m.scrollPageDown()
		return m, cmd
	case "home", "g":
		m.scrollToTop()
		return m, nil
	case "end", "G":
		cmd := m.scrollToBottom()
		return m, cmd
	}
	return m, nil
}

func (m *model) View() string {
	if len(m.messages) == 0 {
		return ""
	}

	m.updateScrollState()
	// Release the slack subscription once it's no longer needed. Starting it
	// is only done from Update via handleAnimationTick, where the returned
	// tea.Cmd can be propagated to actually schedule the next tick.
	if m.bottomSlack == 0 {
		m.slackAnimationSub.Stop()
	}

	if m.totalHeight == 0 {
		return ""
	}

	frameKey := m.transcriptFrameKey()
	if m.lastFrameOutput != "" && frameKey == m.lastFrameKey {
		return m.lastFrameOutput
	}

	// Use virtual total height; a segmented active suffix is intentionally not
	// flattened into renderedLines.
	totalLines := m.totalHeight + m.bottomSlack
	if totalLines == 0 {
		return ""
	}

	startLine := m.scrollOffset
	endLine := min(startLine+m.height, totalLines)

	if startLine >= endLine {
		return ""
	}

	// Copy only the visible window to avoid mutating cached lines
	// This is O(viewportHeight) instead of O(totalHeight)
	visibleLines := make([]string, endLine-startLine)
	for i := startLine; i < endLine; i++ {
		visibleLines[i-startLine] = m.renderedLine(i)
	}

	if m.selection.active {
		visibleLines = m.applySelectionHighlight(visibleLines, startLine)
	}

	m.applyReferenceHover(visibleLines, startLine)
	visibleLines = m.applyURLUnderline(visibleLines, startLine)
	visibleLines = m.applyCopiedFlash(visibleLines, startLine)

	// Sync scroll state and delegate rendering to scrollview which guarantees
	// fixed-width padding, pinned scrollbar, and exact height. Selection and
	// URL-hover restyling preserve display width, so the scrollview can reuse
	// memoized line widths instead of re-measuring every visible line.
	m.scrollview.SetContent(m.renderedLines, m.totalScrollableHeight())
	m.scrollview.SetScrollOffset(m.scrollOffset)
	// Segmented active lines are not in scrollview's flattened content buffer,
	// so use its pre-sliced path. Selection/URL restyling already requires the
	// same viewport-local width work.
	if m.activeSegments != nil && !m.selection.active && m.hoveredURL == nil && m.copiedFlash == nil {
		contentWidth := m.contentWidth()
		for i, line := range visibleLines {
			switch width := ansi.StringWidth(line); {
			case width > contentWidth:
				visibleLines[i] = ansi.Truncate(line, contentWidth, "")
			case width < contentWidth:
				visibleLines[i] = line + strings.Repeat(" ", contentWidth-width)
			}
		}
		return m.finishFrame(frameKey, m.scrollview.ViewWithPaddedLines(visibleLines))
	}
	return m.finishFrame(frameKey, m.scrollview.ViewWithRestyledLines(visibleLines))
}

func (m *model) transcriptFrameKey() transcriptFrameKey {
	frame := transcriptFrameKey{
		scrollbarDragging:        m.scrollview.IsDragging(),
		referenceHoverGeneration: m.referenceHoverGeneration,
		contentGeneration:        m.contentGeneration, segmentsRevision: m.segmentsRevision,
		width: m.width, height: m.height, offset: m.scrollOffset,
		total: m.totalHeight, slack: m.bottomSlack,
		selection: visualSelectionKey{
			active: m.selection.active, startLine: m.selection.startLine,
			startCol: m.selection.startCol, endLine: m.selection.endLine, endCol: m.selection.endCol,
		},
	}
	if m.hoveredURL != nil {
		frame.hoveredURL, frame.hasHoveredURL = *m.hoveredURL, true
	}
	if m.copiedFlash != nil {
		frame.copiedFlash, frame.hasCopiedFlash = *m.copiedFlash, true
	}
	return frame
}

func (m *model) finishFrame(frame transcriptFrameKey, output string) string {
	// Retain only the immediately preceding frame: repeated View calls hit,
	// while any intervening visual state is rendered rather than retained.
	m.lastFrameKey, m.lastFrameOutput = frame, output
	return output
}

func (m *model) renderedLine(global int) string {
	if global < 0 {
		return ""
	}
	if s := m.activeSegments; s != nil && global >= s.start && global < s.start+s.height() {
		return s.line(global - s.start)
	}
	if global < len(m.renderedLines) {
		return m.renderedLines[global]
	}
	return ""
}

// updateScrollState recomputes rendered content, bottom slack and scroll
// offset from the current state of the message list. Called both from View()
// and from Update() on animation ticks so that the slack subscription is
// registered before tui.go schedules the next tick.
func (m *model) updateScrollState() {
	beforeOffset := m.scrollOffset
	defer func() {
		if beforeOffset != m.scrollOffset {
			m.CancelReferenceHover()
		}
	}()
	prevTotalHeight := m.totalHeight
	prevScrollableHeight := m.totalHeight + m.bottomSlack
	m.ensureAllItemsRendered()

	if m.userHasScrolled {
		m.bottomSlack = 0
	} else {
		delta := m.totalHeight - prevTotalHeight
		switch {
		case delta < 0:
			// Cap so the viewport is never mostly empty after a large
			// shrinkage (e.g., several tool calls fading out at once).
			m.bottomSlack = min(m.bottomSlack-delta, m.maxBottomSlack())
		case delta > 0 && m.bottomSlack > 0:
			m.bottomSlack = max(0, m.bottomSlack-delta)
		}
	}

	scrollableHeight := m.totalHeight + m.bottomSlack
	maxScrollOffset := max(0, scrollableHeight-m.height)

	// Auto-scroll when content grows beyond any slack.
	if !m.userHasScrolled && scrollableHeight > prevScrollableHeight {
		m.scrollOffset = maxScrollOffset
	} else {
		m.scrollOffset = max(0, min(m.scrollOffset, maxScrollOffset))
	}
}

// maxBottomSlack returns the maximum blank lines added after content shrinks.
// Small enough that the viewport never feels empty, large enough to absorb a
// typical tool fade-out (~2 lines) without a visible jump.
func (m *model) maxBottomSlack() int {
	return max(1, min(5, m.height/3))
}

// handleAnimationTick refreshes scroll state, decays leftover slack at its
// own cadence, and keeps the slack subscription alive while slack > 0 so
// further ticks fire even after fade animations finish. Returns the command
// to schedule the next tick when the subscription transitions to active.
func (m *model) handleAnimationTick(tick animation.TickMsg) tea.Cmd {
	m.updateScrollState()
	if !m.userHasScrolled && m.bottomSlack > 0 {
		before, after := tick.ElapsedBounds()
		m.bottomSlackElapsed += after - before
		const slackStepDuration = time.Second / 14
		steps := int(m.bottomSlackElapsed / slackStepDuration)
		m.bottomSlackElapsed %= slackStepDuration
		if steps > 0 {
			m.bottomSlack = max(0, m.bottomSlack-steps)
			tick.MarkDirty()
		}
	}
	if m.bottomSlack > 0 {
		return m.slackAnimationSub.Start()
	}
	m.slackAnimationSub.Stop()
	m.bottomSlackElapsed = 0
	return nil
}

// SetSize sets the dimensions of the component
func (m *model) SetSize(width, height int) tea.Cmd {
	if m.width == width && m.height == height {
		return nil // Dimensions unchanged — skip expensive cache invalidation
	}
	m.cancelImageClick()
	m.CancelReferenceHover()
	widthChanged := m.width != width
	m.width = width
	m.height = height

	m.scrollview.SetSize(width, height)
	var cmds []tea.Cmd
	if widthChanged {
		cmds = append(cmds, m.materializeDeferredTail())
		contentWidth := m.contentWidth()
		for _, view := range m.views {
			cmds = append(cmds, view.SetSize(contentWidth, 0))
		}
		m.invalidateAllItems()
	} else {
		cmds = append(cmds, m.materializeDeferredTailForRange(m.scrollOffset, m.scrollOffset+height))
		if !m.userHasScrolled {
			m.scrollOffset = max(0, m.totalScrollableHeight()-height)
		} else {
			m.scrollOffset = min(m.scrollOffset, max(0, m.totalScrollableHeight()-height))
		}
		m.scrollview.SetScrollOffset(m.scrollOffset)
	}
	m.visualGeneration++
	return tea.Batch(cmds...)
}

func (m *model) SetPosition(x, y int) tea.Cmd {
	if m.xPos == x && m.yPos == y {
		return nil
	}
	m.cancelImageClick()
	m.CancelReferenceHover()
	m.xPos = x
	m.yPos = y
	m.scrollview.SetPosition(x, y)
	m.visualGeneration++
	return nil
}

// GetSize returns the current dimensions
func (m *model) GetSize() (width, height int) {
	return m.width, m.height
}

// Focus gives focus to the component.
func (m *model) Focus() tea.Cmd {
	m.focused = true
	// Start selection on the last assistant message for better UX
	m.selectedMessageIndex = m.findLastAssistantMessage()
	if m.selectedMessageIndex < 0 {
		// Fall back to last selectable if no assistant messages
		m.selectedMessageIndex = m.findLastSelectableMessage()
	}
	// Only invalidate the newly selected message
	if m.selectedMessageIndex >= 0 {
		m.invalidateItem(m.selectedMessageIndex)
	}
	m.renderDirty = true
	return nil
}

// Blur removes focus from the component
func (m *model) Blur() tea.Cmd {
	m.cancelImageClick()
	oldIndex := m.selectedMessageIndex
	m.focused = false
	m.selectedMessageIndex = -1
	// Only invalidate the previously selected message
	if oldIndex >= 0 {
		m.invalidateItem(oldIndex)
	}
	m.renderDirty = true
	return nil
}

// FocusAt gives focus and selects the message at the given screen coordinates.
func (m *model) FocusAt(x, y int) tea.Cmd {
	m.focused = true

	oldIndex := m.selectedMessageIndex

	line, _ := m.mouseToLineCol(x, y)
	msgIdx, _, materializeCmd := m.globalLineToMessageLine(line)
	if msgIdx >= 0 && m.isSelectableMessage(msgIdx) {
		m.selectedMessageIndex = msgIdx
	} else {
		m.selectedMessageIndex = m.findLastAssistantMessage()
		if m.selectedMessageIndex < 0 {
			m.selectedMessageIndex = m.findLastSelectableMessage()
		}
	}

	// Only invalidate the old and new selected messages
	if oldIndex >= 0 {
		m.invalidateItem(oldIndex)
	}
	if m.selectedMessageIndex >= 0 && m.selectedMessageIndex != oldIndex {
		m.invalidateItem(m.selectedMessageIndex)
	}
	m.renderDirty = true

	return materializeCmd
}

// Bindings returns key bindings for the component
func (m *model) Bindings() []key.Binding {
	// Return editing bindings when inline editing is active
	if m.inlineEditMsgIndex >= 0 {
		return m.InlineEditBindings()
	}

	bindings := []key.Binding{
		key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "select prev")),
		key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "select next")),
		key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy message")),
		key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
		key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDown", "page down")),
		key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("Home/g", "scroll top")),
		key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("End/G", "scroll bottom")),
	}

	// Only show edit binding when a user message with session position is selected
	if m.selectedMessageIndex >= 0 && m.selectedMessageIndex < len(m.messages) {
		msg := m.messages[m.selectedMessageIndex]
		if msg.Type == types.MessageTypeUser && msg.SessionPosition != nil {
			bindings = append(bindings, key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit message")))
		}
	}

	return bindings
}

// InlineEditBindings returns key bindings for inline edit mode
func (m *model) InlineEditBindings() []key.Binding {
	newlineKeys := m.inlineEditTextarea.KeyMap.InsertNewline.Keys()
	send := core.GetKeys().EditorSend
	send.SetHelp(strings.Join(send.Keys(), "/"), "save")
	return []key.Binding{
		send,
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "cancel")),
		key.NewBinding(key.WithKeys(newlineKeys...), key.WithHelp(strings.Join(newlineKeys, "/"), "newline")),
	}
}

// Help returns the help information
func (m *model) Help() help.KeyMap {
	return core.NewSimpleHelp(m.Bindings())
}

// Scrolling methods
// invalidateView must be called after any state change that can affect View output.
func (m *model) invalidateView() { m.visualGeneration++ }

func (m *model) invalidateFrameContent() {
	m.contentGeneration++
	m.segmentsRevision++
	m.lastFrameOutput = ""
	// Incremental refresh may reuse the same backing array with different widths.
	m.scrollview.InvalidateComposeCache()
}

func (m *model) RenderedContentHeight() int {
	m.ensureAllItemsRendered()
	return m.totalHeight
}

func (m *model) VisualGeneration() uint64 { return m.visualGeneration }

// ResizeCacheStats reports event-loop-owned, monotonic transcript work counters.
func (m *model) ResizeCacheStats() (rebuilds, misses, renderedMessages uint64) {
	return m.transcriptRebuilds, m.itemMisses, m.renderedMessages
}

const (
	defaultScrollAmount = 1
	wheelScrollAmount   = 2
)

func (m *model) scrollUp() {
	if m.scrollOffset > 0 {
		m.userHasScrolled = true
		m.bottomSlack = 0
		m.setScrollOffset(max(0, m.scrollOffset-defaultScrollAmount))
	}
}

func (m *model) scrollDown() tea.Cmd {
	cmd := m.materializeDeferredTailForRange(m.scrollOffset, m.scrollOffset+m.height+defaultScrollAmount)
	m.setScrollOffset(m.scrollOffset + defaultScrollAmount)
	if m.isAtBottom() {
		m.userHasScrolled = false
	}
	return cmd
}

func (m *model) scrollPageUp() {
	m.userHasScrolled = true
	m.bottomSlack = 0
	m.setScrollOffset(max(0, m.scrollOffset-m.height))
}

func (m *model) scrollPageDown() tea.Cmd {
	cmd := m.materializeDeferredTailForRange(m.scrollOffset, m.scrollOffset+m.height*2)
	m.setScrollOffset(m.scrollOffset + m.height)
	if m.isAtBottom() {
		m.userHasScrolled = false
	}
	return cmd
}

func (m *model) scrollToTop() {
	m.userHasScrolled = true
	m.bottomSlack = 0
	m.setScrollOffset(0)
}

func (m *model) materializeDeferredTail() tea.Cmd {
	if len(m.deferredTail) == 0 || m.deferredTailIndex < 0 || m.deferredTailIndex >= len(m.messages) {
		return nil
	}
	msg := m.messages[m.deferredTailIndex]
	var b strings.Builder
	b.Grow(len(msg.Content) + deferredBytes(m.deferredTail))
	b.WriteString(msg.Content)
	for _, chunk := range m.deferredTail {
		b.WriteString(chunk)
	}
	msg.Content = b.String()
	index := m.deferredTailIndex
	cmd := m.views[index].(message.Model).SetMessage(msg)
	m.deferredTail = nil
	m.deferredTailIndex = -1
	m.refreshRenderedItem(index)
	m.visualGeneration++
	return cmd
}

// materializeDeferredTailForRange reconciles stale geometry only when a
// requested viewport/overscan range can reach the deferred final item. The
// cached line offset is the start of that item and remains valid while chunks
// are deferred; if geometry has not been built yet, materialize conservatively.
//
//nolint:unparam // Range shape is kept explicit for viewport callers.
func (m *model) materializeDeferredTailForRange(start, end int) tea.Cmd {
	if len(m.deferredTail) == 0 {
		return nil
	}
	if m.deferredTailIndex < 0 || m.deferredTailIndex >= len(m.lineOffsets) || end > m.lineOffsets[m.deferredTailIndex] {
		return m.materializeDeferredTail()
	}
	return nil
}

func (m *model) materializeDeferredTailForInteraction() tea.Cmd {
	if len(m.deferredTail) != 0 {
		cmd := m.materializeDeferredTail()
		m.updateScrollState()
		m.scrollview.SetContent(m.renderedLines, m.totalScrollableHeight())
		m.scrollview.SetScrollOffset(m.scrollOffset)
		return cmd
	}
	return nil
}

// FinalizeStream establishes the exact externally visible content boundary
// even when the user remains scrolled above the active response.
func (m *model) FinalizeStream() tea.Cmd {
	return m.materializeDeferredTailForInteraction()
}

func deferredBytes(chunks []string) int {
	n := 0
	for _, chunk := range chunks {
		n += len(chunk)
	}
	return n
}

func (m *model) scrollToBottom() tea.Cmd {
	hadDeferredTail := len(m.deferredTail) != 0
	cmd := m.materializeDeferredTail()
	m.userHasScrolled = false
	// A non-deferred final item may still be stale (for example after a hover
	// transition). Materialization already refreshed a deferred item, so never
	// render it a second time at this re-entry boundary.
	if !hadDeferredTail && len(m.views) > 0 {
		m.refreshRenderedItem(len(m.views) - 1)
	}
	m.setScrollOffset(9_999_999) // Will be clamped in View()
	return cmd
}

func (m *model) scrollByWheel(delta int) tea.Cmd {
	if delta == 0 {
		return nil
	}
	var cmd tea.Cmd
	if delta > 0 {
		requestedEnd := m.scrollOffset + m.height + delta*wheelScrollAmount*defaultScrollAmount
		cmd = m.materializeDeferredTailForRange(m.scrollOffset, requestedEnd)
	}

	prevOffset := m.scrollOffset
	m.setScrollOffset(m.scrollOffset + (delta * wheelScrollAmount * defaultScrollAmount))
	if m.scrollOffset == prevOffset {
		return cmd
	}

	if delta < 0 {
		m.userHasScrolled = true
		m.bottomSlack = 0
	} else if m.isAtBottom() {
		m.userHasScrolled = false
	}
	return cmd
}

func (m *model) setScrollOffset(offset int) {
	before := m.scrollOffset
	maxOffset := max(0, m.totalScrollableHeight()-m.height)
	m.scrollOffset = max(0, min(offset, maxOffset))
	m.scrollview.SetScrollOffset(m.scrollOffset)
	if before != m.scrollOffset {
		m.CancelReferenceHover()
		m.cancelImageClick()
		m.invalidateView()
	}
}

func (m *model) isAtBottom() bool {
	if len(m.messages) == 0 {
		return true
	}
	maxScrollOffset := max(0, m.totalScrollableHeight()-m.height)
	return m.scrollOffset >= maxScrollOffset
}

// Message selection methods
func (m *model) isSelectableMessage(index int) bool {
	if index < 0 || index >= len(m.messages) {
		return false
	}
	msg := m.messages[index]
	switch msg.Type {
	case types.MessageTypeAssistant, types.MessageTypeAssistantReasoningBlock:
		return true
	case types.MessageTypeUser:
		// User messages are selectable only if they have a session position (editable)
		return msg.SessionPosition != nil
	default:
		return false
	}
}

func (m *model) findLastSelectableMessage() int {
	for i := range slices.Backward(m.messages) {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

// findLastAssistantMessage finds the last assistant or reasoning block message.
// Used for initial focus selection to start on assistant content.
func (m *model) findLastAssistantMessage() int {
	for i := range slices.Backward(m.messages) {
		msg := m.messages[i]
		if msg.Type == types.MessageTypeAssistant || msg.Type == types.MessageTypeAssistantReasoningBlock {
			return i
		}
	}
	return -1
}

func (m *model) findPreviousSelectableMessage(fromIndex int) int {
	for i := fromIndex - 1; i >= 0; i-- {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

func (m *model) findNextSelectableMessage(fromIndex int) int {
	for i := fromIndex + 1; i < len(m.messages); i++ {
		if m.isSelectableMessage(i) {
			return i
		}
	}
	return -1
}

func (m *model) selectPreviousMessage() {
	if len(m.messages) == 0 {
		return
	}
	if prevIndex := m.findPreviousSelectableMessage(m.selectedMessageIndex); prevIndex >= 0 {
		oldIndex := m.selectedMessageIndex
		m.selectedMessageIndex = prevIndex
		if oldIndex >= 0 {
			m.invalidateItem(oldIndex)
		}
		m.invalidateItem(prevIndex)
		m.renderDirty = true
		m.scrollToSelectedMessage()
	}
}

func (m *model) selectNextMessage() {
	if len(m.messages) == 0 {
		return
	}
	if nextIndex := m.findNextSelectableMessage(m.selectedMessageIndex); nextIndex >= 0 {
		oldIndex := m.selectedMessageIndex
		m.selectedMessageIndex = nextIndex
		if oldIndex >= 0 {
			m.invalidateItem(oldIndex)
		}
		m.invalidateItem(nextIndex)
		m.renderDirty = true
		m.scrollToSelectedMessage()
	}
}

func (m *model) scrollToSelectedMessage() {
	if m.selectedMessageIndex < 0 || m.selectedMessageIndex >= len(m.messages) {
		return
	}

	// Ensure all items are rendered so lineOffsets and totalHeight are accurate
	m.ensureAllItemsRendered()

	if m.selectedMessageIndex >= len(m.lineOffsets) {
		return
	}

	startLine := m.lineOffsets[m.selectedMessageIndex]

	var selectedHeight int
	if m.selectedMessageIndex < len(m.views) {
		item := m.renderItem(m.selectedMessageIndex, m.views[m.selectedMessageIndex])
		selectedHeight = item.height
	}
	endLine := startLine + selectedHeight

	// Scroll to show the top of the selected message.
	// When messages are taller than the viewport, always anchor to the start
	// so the user sees the beginning of the message first.
	if startLine < m.scrollOffset || endLine > m.scrollOffset+m.height {
		m.setScrollOffset(startLine)
	}
}

// Caching methods
func (m *model) shouldCacheMessage(index int) bool {
	if index < 0 || index >= len(m.messages) {
		return false
	}

	msg := m.messages[index]
	switch msg.Type {
	case types.MessageTypeToolCall:
		// Mutations invalidate this item; animation ticks evict live frames.
		return true
	case types.MessageTypeToolResult:
		return true
	case types.MessageTypeAssistant:
		return strings.Trim(msg.Content, "\r\n\t ") != "" || len(msg.AssistantMedia) > 0
	case types.MessageTypeAssistantReasoningBlock:
		// Cacheable once spinners/fades have settled. Content mutations go
		// through invalidateItem, which drops any stale entry.
		if index < len(m.views) {
			if block, ok := m.views[index].(*reasoningblock.Model); ok {
				return !block.NeedsTick()
			}
		}
		return false
	case types.MessageTypeUser, types.MessageTypeAgentInput, types.MessageTypeRuntimeNotice:
		return true
	default:
		return false
	}
}

func (m *model) renderItem(index int, view layout.Model) renderedItem {
	// If this message is being inline edited, render the textarea instead
	if index == m.inlineEditMsgIndex {
		rendered := m.renderInlineEditTextarea()
		var lines []string
		if rendered != "" {
			lines = strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
		}
		return renderedItem{lines: lines, height: len(lines)}
	}

	isSelected := m.focused && index == m.selectedMessageIndex
	isHovered := index == m.hoveredMessageIndex

	switch v := view.(type) {
	case message.Model:
		v.SetSelected(isSelected)
		v.SetHovered(isHovered)
	case *reasoningblock.Model:
		v.SetSelected(isSelected)
	}

	shouldCache := !isSelected && !isHovered && m.shouldCacheMessage(index)
	if shouldCache {
		if cached, exists := m.renderedItems[index]; exists {
			if m.activeSegments != nil && m.activeSegments.index == index {
				segments := m.activeSegments
				return renderedItem{segments: &message.AssistantSegments{Header: segments.header, Stable: segments.stable, Tail: segments.tail}, height: cached.height}
			}
			if cached.start+cached.height <= len(m.renderedLines) {
				return renderedItem{lines: m.renderedLines[cached.start : cached.start+cached.height], height: cached.height}
			}
		}
	}

	m.itemMisses++
	m.renderedMessages++
	if v, ok := view.(message.Model); ok {
		if segments, ok := v.RenderedSegments(m.contentWidth()); ok {
			item := renderedItem{segments: &segments, height: len(segments.Header) + len(segments.Stable) + len(segments.Tail)}
			return item
		}
	}
	rendered := view.View()
	var lines []string
	if rendered != "" {
		lines = strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	}

	item := renderedItem{lines: lines, height: len(lines)}

	return item
}

// renderInlineEditTextarea renders the inline editing textarea with user message styling.
func (m *model) renderInlineEditTextarea() string {
	// Use the same style as user messages but with a highlight to indicate editing
	editStyle := styles.UserMessageStyle.
		BorderForeground(styles.Accent)

	innerWidth := m.contentWidth() - editStyle.GetHorizontalFrameSize()
	if innerWidth > 0 {
		m.inlineEditTextarea.SetWidth(innerWidth)
	}

	// The textarea is set to a large height to prevent internal viewport scrolling
	// which causes cursor positioning bugs in multi-line content. We trim the
	// end-of-buffer padding lines from the rendered output.
	view := m.inlineEditTextarea.View()
	view = trimEndOfBufferLines(view)

	// Add a minimal edit indicator at the bottom left with extra padding
	editHint := styles.MutedStyle.Render("[editing]")

	content := view + "\n\n" + editHint
	return editStyle.Width(m.contentWidth()).Render(content)
}

// trimEndOfBufferLines removes trailing end-of-buffer padding lines from a
// textarea's rendered View output. The textarea pads its view to fill its
// configured height; these padding lines contain only whitespace (after
// stripping ANSI sequences) and appear after the actual content.
func trimEndOfBufferLines(view string) string {
	lines := strings.Split(view, "\n")

	// Trim trailing lines that are visually empty (whitespace-only after ANSI strip).
	// Content lines always contain visible text or cursor escape sequences.
	// Always keep at least one line so that an empty textarea still renders
	// the cursor line instead of returning the full padded view.
	last := len(lines)
	for last > 1 && strings.TrimSpace(ansi.Strip(lines[last-1])) == "" {
		last--
	}

	return strings.Join(lines[:last], "\n")
}

func (m *model) needsSeparator(index int) bool {
	if index >= len(m.messages)-1 {
		return false
	}
	currentIsToolCall := m.messages[index].Type == types.MessageTypeToolCall
	nextIsToolCall := m.messages[index+1].Type == types.MessageTypeToolCall

	// Always add a separator before transfer_task, even between consecutive tool calls
	if nextIsToolCall && m.messages[index+1].ToolCall.Function.Name == transfertask.ToolNameTransferTask {
		return true
	}

	return !currentIsToolCall || !nextIsToolCall
}

func (m *model) ensureAllItemsRendered() {
	if m.replay != nil {
		return
	}
	if generation := styles.ThemeGeneration(); m.themeGeneration != generation {
		m.themeGeneration = generation
		if m.inlineEditMsgIndex >= 0 {
			m.inlineEditTextarea.SetStyles(inlineEditStyles())
		}
		m.InvalidateRenderCaches()
	}
	if !m.renderDirty && (len(m.renderedLines) > 0 || m.activeSegments != nil) {
		return
	}

	m.transcriptRebuilds++
	m.invalidateFrameContent()
	if len(m.views) == 0 {
		m.CancelReferenceHover()
		m.renderedLines = nil
		m.totalHeight = 0
		m.renderDirty = false
		return
	}

	// Cached ranges still read the old backing array while the replacement grows.
	allLines := make([]string, 0, len(m.renderedLines))
	var activeSegments *activeTranscriptSegments
	ranges := make(renderedItemIndex, len(m.views))
	offsets := make([]int, len(m.views))
	virtualHeight := 0

	for i, view := range m.views {
		offsets[i] = virtualHeight
		item := m.renderItem(i, view)
		if m.shouldCacheMessage(i) && (!m.focused || i != m.selectedMessageIndex) && i != m.hoveredMessageIndex {
			ranges[i] = renderedItemRange{start: virtualHeight, height: item.height}
		}
		if item.height == 0 {
			continue
		}
		if item.segments != nil && i == len(m.views)-1 {
			activeSegments = &activeTranscriptSegments{index: i, start: virtualHeight, header: item.segments.Header, stable: item.segments.Stable, tail: item.segments.Tail}
			virtualHeight += item.height
		} else {
			if item.segments != nil {
				allLines = append(allLines, item.segments.Header...)
				allLines = append(allLines, item.segments.Stable...)
				allLines = append(allLines, item.segments.Tail...)
			} else {
				allLines = append(allLines, item.lines...)
			}
			virtualHeight += item.height
		}

		if m.needsSeparator(i) {
			allLines = append(allLines, "")
			virtualHeight++
		}
	}

	if len(m.lineOffsets) > 0 && (m.totalHeight != virtualHeight || !slices.Equal(m.lineOffsets, offsets)) {
		m.CancelReferenceHover()
	}
	m.pruneReferenceHover()
	m.renderedLines = allLines
	m.activeSegments = activeSegments
	m.renderedItems = ranges
	m.lineOffsets = offsets
	m.totalHeight = virtualHeight
	m.urlSpans.clear()
	m.renderDirty = false
}

func (m *model) refreshRenderedItem(index int) bool {
	m.invalidateFrameContent()
	wasAtBottom := m.isAtBottom()
	if m.renderDirty || (len(m.renderedLines) == 0 && m.activeSegments == nil) || len(m.lineOffsets) != len(m.views) || index < 0 || index >= len(m.views) {
		m.invalidateItem(index)
		return false
	}
	start := m.lineOffsets[index]
	end := m.totalHeight
	if index+1 < len(m.lineOffsets) {
		end = m.lineOffsets[index+1]
	}
	if m.needsSeparator(index) && end > start && m.renderedLine(end-1) == "" {
		end--
	}
	m.renderedItems.Delete(index)
	item := m.renderItem(index, m.views[index])
	if item.segments != nil && index == len(m.views)-1 {
		// The final assistant has exactly one line owner. It may previously have
		// been flattened (selection/full Render) or virtual (stream segmentation),
		// so discard every flattened line at and after its canonical offset before
		// installing the segmented representation. Keeping either the old flattened
		// suffix or a shortened prefix makes renderedLines, activeSegments and
		// totalHeight describe incompatible coordinate spaces.
		if start < 0 || start > len(m.renderedLines) {
			m.renderDirty = true
			return false
		}
		m.renderedLines = m.renderedLines[:start]
		m.activeSegments = &activeTranscriptSegments{index: index, start: start, header: item.segments.Header, stable: item.segments.Stable, tail: item.segments.Tail}
		m.totalHeight = start + item.height
		if m.shouldCacheMessage(index) {
			m.renderedItems[index] = renderedItemRange{start: start, height: item.height}
		}
		if wasAtBottom && !m.userHasScrolled {
			m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
		} else {
			m.scrollOffset = min(m.scrollOffset, max(0, m.totalScrollableHeight()-m.height))
		}
		m.scrollview.SetScrollOffset(m.scrollOffset)
		m.hoveredURL = nil
		m.urlSpans.clear()
		return true
	}
	if start < 0 || end < start || end > len(m.renderedLines) {
		m.renderDirty = true
		return false
	}
	// A fallback/full rendering replaces any segmented suffix.
	if m.activeSegments != nil && m.activeSegments.index == index {
		prefix := make([]string, 0, len(m.renderedLines)+m.activeSegments.height())
		prefix = append(prefix, m.renderedLines...)
		prefix = append(prefix, m.activeSegments.header...)
		prefix = append(prefix, m.activeSegments.stable...)
		prefix = append(prefix, m.activeSegments.tail...)
		m.renderedLines = prefix
		m.activeSegments = nil
		end = len(m.renderedLines)
	}
	// Every non-virtual item is flattened through the same line source used by a
	// full rebuild. RenderedSegments is available for historical assistants too;
	// splicing item.lines directly would therefore replace that message with zero
	// lines on hover and leave offsets/totalHeight pointing into blank space.
	itemLines := m.renderedItemLines(item)
	// Replace the final item in place. It is normally the transcript suffix, so
	// reslicing avoids copying the entire historical prefix on every streamed
	// chunk; append only copies if the tail outgrows retained capacity.
	if index == len(m.views)-1 && end == len(m.renderedLines) {
		m.renderedLines = append(m.renderedLines[:start], itemLines...)
	} else {
		replacement := make([]string, 0, len(m.renderedLines)-(end-start)+item.height)
		replacement = append(replacement, m.renderedLines[:start]...)
		replacement = append(replacement, itemLines...)
		replacement = append(replacement, m.renderedLines[end:]...)
		m.renderedLines = replacement
	}
	delta := item.height - (end - start)
	if delta != 0 {
		m.CancelReferenceHover()
	}
	for i := index + 1; i < len(m.lineOffsets); i++ {
		m.lineOffsets[i] += delta
	}
	m.totalHeight += delta
	for i, cached := range m.renderedItems {
		if i > index {
			cached.start += delta
			m.renderedItems[i] = cached
		}
	}
	if m.shouldCacheMessage(index) && (!m.focused || index != m.selectedMessageIndex) && index != m.hoveredMessageIndex {
		m.renderedItems[index] = renderedItemRange{start: start, height: item.height}
	}
	if wasAtBottom && !m.userHasScrolled {
		m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
	} else {
		m.scrollOffset = min(m.scrollOffset, max(0, m.totalScrollableHeight()-m.height))
	}
	m.scrollview.SetScrollOffset(m.scrollOffset)
	m.hoveredURL = nil
	m.urlSpans.clear()
	return true
}

func (m *model) refreshHoverItems(indices ...int) {
	for _, index := range indices {
		if index >= 0 {
			m.refreshRenderedItem(index)
		}
	}
}

func (m *model) invalidateItem(index int) {
	// Delete unconditionally: cacheability is state-dependent (e.g. a settled
	// reasoning block becomes animated again), so gating the delete on
	// shouldCacheMessage could leave a stale entry behind.
	m.renderedItems.Delete(index)
	m.renderDirty = true
	m.invalidateView()
}

func (m *model) invalidateAllItems() {
	m.invalidateFrameContent()
	m.renderedItems.Clear()
	m.activeSegments = nil
	m.renderedLines = nil
	m.lineOffsets = nil
	m.totalHeight = 0
	m.urlSpans.clear()
	m.renderDirty = true
	m.invalidateView()
}

func (m *model) InvalidateRenderCaches() {
	for _, view := range m.views {
		if mv, ok := view.(message.Model); ok {
			mv.InvalidateRenderCache()
		}
	}
	m.invalidateAllItems()
}

// finalizePreviousMessageView releases per-message render state on the most
// recent message.Model view (if any) before a new top-level entry is
// appended. The flattened transcript owns historical lines; only the active assistant
// needs a markdown renderer or a second per-message rendering cache.
func (m *model) finalizePreviousMessageView() {
	if len(m.views) == 0 {
		return
	}
	if mv, ok := m.views[len(m.views)-1].(message.Model); ok {
		mv.Finalize()
	}
}

// Message management methods
func (m *model) AddUserMessage(content string) tea.Cmd {
	return m.addMessage(types.User(content))
}

func (m *model) AddLoadingMessage(description string) tea.Cmd {
	return m.addMessage(types.Loading(description))
}

func (m *model) ReplaceLoadingWithUser(content string, sessionPos int) tea.Cmd {
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeLoading {
			m.messages = slices.Delete(m.messages, i, i+1)
			if i < len(m.views) {
				m.views = slices.Delete(m.views, i, i+1)
			}
			m.invalidateAllItems()
			break
		}
	}
	msg := types.User(content)
	if sessionPos >= 0 {
		pos := sessionPos
		msg.SessionPosition = &pos
	}
	return m.addMessage(msg)
}

func (m *model) AddInputMessage(msg *session.Message, sessionPos int) tea.Cmd {
	if !lifecycle.VisibleTranscriptMessage(msg) {
		return nil
	}
	if !lifecycle.IsUserInput(msg.InputOrigin) {
		return m.addMessage(types.Input(msg))
	}
	return m.ReplaceLoadingWithUser(msg.Message.Content, sessionPos)
}

func (m *model) AddErrorMessage(content string) tea.Cmd {
	m.removeSpinner()
	return m.addMessage(types.Error(content))
}

func (m *model) AddShellOutputMessage(content string) tea.Cmd {
	return m.addMessage(types.ShellOutput(content))
}

// AddAgentReturn appends the delegation-return transition. A pending-response
// spinner pinned at the tail is re-added (with its label) after the
// transition so the "spinner is always last" invariant — relied on by
// RemoveSpinner — keeps holding.
func (m *model) AddAgentReturn(fromAgent, toAgent string) tea.Cmd {
	if fromAgent == "" || toAgent == "" {
		return nil
	}
	var trailingSpinner *types.Message
	if last := m.lastMessage(); last != nil && last.Type == types.MessageTypeSpinner {
		trailingSpinner = last
		m.removeSpinner()
	}
	cmds := []tea.Cmd{m.addMessage(types.AgentReturn(fromAgent, toAgent))}
	if trailingSpinner != nil {
		cmds = append(cmds, m.addMessage(types.SpinnerLabeled(trailingSpinner.Sender, trailingSpinner.Content)))
	}
	return tea.Batch(cmds...)
}

func (m *model) AddAssistantMessage(sender, label string) tea.Cmd {
	m.removeSpinner()
	if label == "" {
		return m.addMessage(types.Spinner())
	}
	return m.addMessage(types.SpinnerLabeled(sender, label))
}

func (m *model) AddCancelledMessage() tea.Cmd {
	m.finalizePreviousMessageView()
	msg := types.Cancelled()
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view.Init()
}

func (m *model) AddWelcomeMessage(content string) tea.Cmd {
	if content == "" || len(m.views) > 0 {
		return nil
	}
	msg := types.Welcome(content)
	m.messages = append(m.messages, msg)
	view := m.createMessageView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true
	return view.Init()
}

func (m *model) addMessage(msg *types.Message) tea.Cmd {
	m.clearSelection()
	shouldAutoScroll := !m.userHasScrolled

	m.finalizePreviousMessageView()
	view := m.createMessageView(msg)
	m.sessionState.SetPreviousMessage(msg)

	// Keep the pending-response spinner pinned at the tail: content that
	// arrives while waiting (e.g. runtime-injected subagent notes) slots in
	// above it, preserving removeSpinner's tail invariant.
	if tail := len(m.messages) - 1; tail >= 0 &&
		msg.Type != types.MessageTypeSpinner &&
		m.messages[tail].Type == types.MessageTypeSpinner {
		m.messages = slices.Insert(m.messages, tail, msg)
		m.views = slices.Insert(m.views, tail, view)
	} else {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, view)
	}
	m.renderDirty = true
	m.invalidateView()

	var cmds []tea.Cmd
	if initCmd := view.Init(); initCmd != nil {
		cmds = append(cmds, initCmd)
	}
	if shouldAutoScroll {
		cmds = append(cmds, func() tea.Msg {
			return scrollToBottomMsg{}
		})
	}

	return tea.Batch(cmds...)
}

// ResetFromSession replaces authoritative content without moving the reader.
func (m *model) ResetFromSession(sess *session.Session, media map[int][]types.AssistantMedia) tea.Cmd {
	offset, scrolled, selected := m.scrollOffset, m.userHasScrolled, m.selectedMessageIndex
	selection := m.selection
	cmd := m.loadFromSession(sess, media, false)
	m.scrollOffset, m.userHasScrolled = offset, scrolled
	m.selectedMessageIndex = min(selected, len(m.messages)-1)
	m.selection = selection

	m.ensureAllItemsRendered()
	if !scrolled {
		m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
	}
	m.scrollOffset = min(m.scrollOffset, max(0, m.totalScrollableHeight()-m.height))
	m.scrollview.SetScrollOffset(m.scrollOffset)
	return cmd
}

func (m *model) LoadFromSession(sess *session.Session, generatedMedia map[int][]types.AssistantMedia) tea.Cmd {
	m.SetImagePreviewSessionID(sess.ID)
	return m.loadFromSession(sess, generatedMedia, true)
}

func (m *model) startSessionReplay(sess *session.Session, generatedMedia map[int][]types.AssistantMedia, toolResults map[string]string) {
	m.inputParentSessionID = sess.ParentID
	if m.subagents == nil {
		m.subagents = subagentindex.New()
	}
	if snapshot := sess.GetSubagentTree(); snapshot != nil {
		m.subagents.Reset(*snapshot)
	}
	appendSessionMessage := func(msg *types.Message, view layout.Model) {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, view)
		m.sessionState.SetPreviousMessage(msg)
	}

	// Each committed assistant owns its reasoning block, even for the same agent.
	newReasoningBlock := func(agentName string) *reasoningblock.Model {
		// Create new reasoning block
		block := reasoningblock.New(m.ar, nextBlockID(), agentName, m.sessionState, m.subagents)
		block.SetShowAgentBadge(showReasoningAgentBadge(m.lastMessage(), agentName))
		block.SetSize(m.contentWidth(), 0)

		blockMsg := &types.Message{
			Type:   types.MessageTypeAssistantReasoningBlock,
			Sender: agentName,
		}
		appendSessionMessage(blockMsg, block)
		return block
	}

	// Historical calls stay inert; authoritative live seeds restore active status.
	// addStandaloneToolCall adds a tool call as a standalone message (not in a reasoning block)
	addStandaloneToolCall := func(agentName string, tc tools.ToolCall, toolDef tools.Tool, toolResults map[string]string) {
		toolMsg := types.ToolCallMessage(agentName, tc, toolDef, types.ToolStatusCompleted)
		// Apply tool result if available
		if result, ok := toolResults[tc.ID]; ok {
			toolMsg.Content = strings.ReplaceAll(result, "\t", "    ")
		}
		view := m.createToolCallView(toolMsg)
		appendSessionMessage(toolMsg, view)
	}

	m.StopAnimations()
	m.messages = nil
	m.views = nil
	m.renderDirty = true
	m.renderedItems.Clear()
	m.activeSegments = nil
	m.renderedLines = nil
	m.scrollOffset = 0
	m.totalHeight = 0
	m.bottomSlack = 0
	m.selectedMessageIndex = -1
	m.hoveredMessageIndex = -1
	m.hoveredURL = nil

	apply := func(pos int, item session.Item) {
		if item.IsError() {
			errMsg := types.Error(item.Error.Message)
			appendSessionMessage(errMsg, m.createMessageView(errMsg))
			return
		}
		if !item.IsMessage() {
			return
		}

		smsg := item.Message
		if !lifecycle.VisibleTranscriptMessage(smsg) {
			return
		}

		switch smsg.Message.Role {
		case chat.MessageRoleUser:
			if !lifecycle.IsUserInput(smsg.InputOrigin) {
				msg := types.Input(smsg)
				appendSessionMessage(msg, m.createMessageView(msg))
				return
			}
			msg := types.User(smsg.Message.Content)
			msgPos := pos
			msg.SessionPosition = &msgPos
			appendSessionMessage(msg, m.createMessageView(msg))
		case chat.MessageRoleAssistant:
			hasReasoning := smsg.Message.ReasoningContent != ""
			hasContent := smsg.Message.Content != ""
			hasToolCalls := len(smsg.Message.ToolCalls) > 0
			var reasoningBlock *reasoningblock.Model

			// Step 1: Handle reasoning content - only create/extend a reasoning block if there's actual reasoning
			if hasReasoning {
				reasoningBlock = newReasoningBlock(smsg.AgentName)
				reasoningBlock.AppendReasoning(smsg.Message.ReasoningContent)
				// Update the message content for copying
				lastIdx := len(m.messages) - 1
				if m.messages[lastIdx].Content != "" {
					m.messages[lastIdx].Content += "\n\n"
				}
				m.messages[lastIdx].Content += smsg.Message.ReasoningContent
			}

			// Step 2: Handle assistant content — this breaks the reasoning
			// block chain. Restored generated media joins the same message
			// (or forms a media-only one), mirroring AppendAssistantMedia's
			// live behavior.
			restoredMedia := generatedMedia[pos]
			if hasContent || len(restoredMedia) > 0 {
				msg := types.Agent(types.MessageTypeAssistant, smsg.AgentName, smsg.Message.Content)
				msg.AssistantMedia = restoredMedia
				appendSessionMessage(msg, m.createMessageView(msg))
			}

			// Step 3: Handle tool calls
			// Tool calls go into the reasoning block ONLY if there was reasoning content AND no regular content
			if hasToolCalls {
				attachToReasoning := reasoningBlock != nil && !hasContent
				for i, tc := range smsg.Message.ToolCalls {
					var toolDef tools.Tool
					if i < len(smsg.Message.ToolDefinitions) {
						toolDef = smsg.Message.ToolDefinitions[i]
					}

					if attachToReasoning {
						toolMsg := types.ToolCallMessage(smsg.AgentName, tc, toolDef, types.ToolStatusCompleted)
						reasoningBlock.AddToolCall(toolMsg)
						if result, ok := toolResults[tc.ID]; ok {
							reasoningBlock.UpdateToolResult(tc.ID, result, types.ToolStatusCompleted, nil)
						}
						continue
					}

					addStandaloneToolCall(smsg.AgentName, tc, toolDef, toolResults)
				}
			}
		case chat.MessageRoleTool:
			return
		}
	}

	m.loadedItemCount = len(sess.Messages)
	m.replay = &sessionReplay{items: sess.Messages, apply: apply}
}

func (m *model) loadFromSession(sess *session.Session, generatedMedia map[int][]types.AssistantMedia, scroll bool) tea.Cmd {
	snapshot := sess.Clone()
	m.startSessionReplay(snapshot, generatedMedia, replayToolResults(snapshot.Messages))
	var cmds []tea.Cmd
	for m.replay != nil {
		_, cmd := m.applySessionReplay(false)
		cmds = append(cmds, cmd)
	}
	if scroll {
		cmds = append(cmds, m.ScrollToBottom())
	}
	return tea.Batch(cmds...)
}

func (m *model) AddOrUpdateToolCall(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status types.ToolStatus) tea.Cmd {
	// First check if this tool call exists in any reasoning block
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeAssistantReasoningBlock {
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if block.HasToolCall(toolCall.ID) {
					block.UpdateToolCall(toolCall.ID, status, toolCall.Function.Arguments)
					m.invalidateItem(i)
					return nil
				}
			}
		}
	}

	// Then try to update existing standalone tool by ID
	for i := range slices.Backward(m.messages) {
		msg := m.messages[i]
		if msg.Type == types.MessageTypeToolCall && msg.ToolCall.ID == toolCall.ID {
			msg.ToolStatus = status
			if status == types.ToolStatusRunning && msg.StartedAt == nil {
				now := time.Now()
				msg.StartedAt = &now
			}
			if toolCall.Function.Arguments != "" {
				if status == types.ToolStatusPending {
					msg.ToolCall.Function.Arguments += toolCall.Function.Arguments
				} else {
					msg.ToolCall.Function.Arguments = toolCall.Function.Arguments
				}
			}
			m.invalidateItem(i)
			return nil
		}
	}

	m.removeSpinner()

	// If there's an active reasoning block, add the tool call to it
	if block, blockIdx := m.getActiveReasoningBlock(agentName); block != nil {
		msg := types.ToolCallMessage(agentName, toolCall, toolDef, status)
		cmd := block.AddToolCall(msg)
		m.invalidateItem(blockIdx)
		return cmd
	}

	// Otherwise create a standalone tool call message
	m.finalizePreviousMessageView()
	msg := types.ToolCallMessage(agentName, toolCall, toolDef, status)
	m.messages = append(m.messages, msg)
	view := m.createToolCallView(msg)
	m.views = append(m.views, view)
	m.renderDirty = true

	return view.Init()
}

func (m *model) AppendToolOutput(msg *runtime.ToolCallOutputEvent) tea.Cmd {
	if msg.Output == "" {
		return nil
	}

	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeAssistantReasoningBlock {
			if block, ok := m.views[i].(*reasoningblock.Model); ok && block.AppendToolOutput(msg.ToolCallID, msg.Output) {
				m.invalidateItem(i)
				return nil
			}
		}
	}

	for i := range slices.Backward(m.messages) {
		toolMessage := m.messages[i]
		if toolMessage.Type != types.MessageTypeToolCall || toolMessage.ToolCall.ID != msg.ToolCallID {
			continue
		}
		toolMessage.AppendToolOutput(msg.Output)
		if toolMessage.ToolStatus == types.ToolStatusPending {
			toolMessage.ToolStatus = types.ToolStatusRunning
			if toolMessage.StartedAt == nil {
				now := time.Now()
				toolMessage.StartedAt = &now
			}
		}
		m.invalidateItem(i)
		return nil
	}

	return nil
}

func (m *model) AddToolResult(msg *runtime.ToolCallResponseEvent, status types.ToolStatus) tea.Cmd {
	// First check reasoning blocks for the tool call
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type == types.MessageTypeAssistantReasoningBlock {
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if block.HasToolCall(msg.ToolCallID) {
					cmd := block.UpdateToolResult(msg.ToolCallID, msg.Response, status, msg.Result)
					m.invalidateItem(i)
					return cmd
				}
			}
		}
	}

	// Then check standalone tool call messages
	for i := range slices.Backward(m.messages) {
		toolMessage := m.messages[i]
		if toolMessage.Type == types.MessageTypeToolCall && toolMessage.ToolCall.ID == msg.ToolCallID {
			toolMessage.Content = strings.ReplaceAll(msg.Response, "\t", "    ")
			toolMessage.ToolStatus = status
			toolMessage.ToolResult = msg.Result.WithoutPayload()
			toolMessage.Images = tuiimage.FromToolResult(msg.Result)
			m.invalidateItem(i)

			// The replaced view may still hold a running-spinner subscription.
			animation.StopView(m.views[i])
			view := m.createToolCallView(toolMessage)
			agentmessage.PreserveExpansion(m.views[i], view)
			m.views[i] = view
			return view.Init()
		}
	}
	return nil
}

// LoadedItemCount reports the length of the item snapshot last rendered by
// LoadFromSession — everything at a session position below it is already on
// screen when merging with a live stream.
func (m *model) LoadedItemCount() int { return m.loadedItemCount }

// FinalizeStreamedAssistant settles the streamed tail of an assistant message
// against its committed content (see the Model interface docs). The streamed
// tail is the trailing run of assistant-text and reasoning-block messages
// from agentName rendered AFTER the transcript snapshot — snapshot bubbles
// are never touched, even when adjacent. Standalone tool bubbles are keyed by
// tool-call id and update themselves, so they are never part of the tail.
func (m *model) FinalizeStreamedAssistant(agentName, content, reasoning string, alreadyShown bool) tea.Cmd {
	materialized := m.materializeDeferredTail()
	defer func() { m.committedMessageCount = len(m.messages) }()
	// Locate the streamed tail, bounded by the snapshot render.
	start := len(m.messages)
	for start > max(m.loadedMessageCount, m.committedMessageCount) {
		msg := m.messages[start-1]
		isStreamed := msg.InputOrigin != session.InputOriginAgent && msg.Sender == agentName &&
			(msg.Type == types.MessageTypeAssistant || msg.Type == types.MessageTypeAssistantReasoningBlock)
		if !isStreamed {
			break
		}
		start--
	}

	if alreadyShown {
		// The committed message came with the transcript snapshot and is on
		// screen; the streamed tail duplicates it.
		if start == len(m.messages) {
			return nil
		}
		m.messages = m.messages[:start]
		m.views = m.views[:start]
		if m.selectedMessageIndex >= len(m.messages) {
			m.selectedMessageIndex = -1
		}
		if m.hoveredMessageIndex >= len(m.messages) {
			m.hoveredMessageIndex = -1
		}
		m.invalidateAllItems()
		m.renderDirty = true
		return nil
	}

	// Finalize the committed message's bubbles in place with the canonical
	// committed content — self-healing for any streaming delta the viewer
	// missed. Scan backwards for the LAST streamed message only: its text
	// bubble, and the reasoning block that opens it (a second text bubble
	// means we crossed into an earlier message).
	textIdx, reasonIdx := -1, -1
scan:
	for i := len(m.messages) - 1; i >= start; i-- {
		switch m.messages[i].Type {
		case types.MessageTypeAssistant:
			if textIdx != -1 {
				break scan
			}
			textIdx = i
		case types.MessageTypeAssistantReasoningBlock:
			reasonIdx = i
			break scan
		}
	}

	cmds := []tea.Cmd{materialized}
	if reasoning != "" {
		if reasonIdx != -1 {
			if block, ok := m.views[reasonIdx].(*reasoningblock.Model); ok {
				block.SetCommittedReasoning(reasoning)
				m.messages[reasonIdx].Content = reasoning
				m.invalidateItem(reasonIdx)
			}
		} else {
			cmds = append(cmds, m.addReasoningBlock(agentName, reasoning))
		}
	}
	if content != "" {
		if textIdx != -1 {
			m.messages[textIdx].Content = content
			m.views[textIdx].(message.Model).SetMessage(m.messages[textIdx])
			m.invalidateItem(textIdx)
		} else {
			cmds = append(cmds, m.addMessage(types.Agent(types.MessageTypeAssistant, agentName, content)))
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) CompleteAssistant(event *runtime.MessageAddedEvent) tea.Cmd {
	start := m.committedMessageCount
	cmd := m.CommitAssistant(event.GetAgentName(), event.CommittedToolCallIDs())
	m.committedMessageCount = min(start, len(m.messages))
	// Tool bubbles may split the streamed text; they already own their content.
	if event.Message != nil && len(event.Message.Message.ToolCalls) == 0 {
		cmd = tea.Batch(cmd, m.FinalizeStreamedAssistant(event.GetAgentName(), event.Message.Message.Content, event.Message.Message.ReasoningContent, false))
	}
	m.committedMessageCount = len(m.messages)
	return cmd
}

// CommitAssistant closes the delta tail and retires only unpublished partial calls.
func (m *model) CommitAssistant(agentName string, toolCallIDs []string) tea.Cmd {
	cmd := m.materializeDeferredTail()
	for i := len(m.messages) - 1; i >= max(m.loadedMessageCount, m.committedMessageCount); i-- {
		msg := m.messages[i]
		if msg.Sender != agentName {
			continue
		}
		if block, ok := m.views[i].(*reasoningblock.Model); ok {
			if block.RemoveUncommittedTools(toolCallIDs) {
				m.invalidateItem(i)
			}
		}
		if msg.Type != types.MessageTypeToolCall || msg.ToolStatus != types.ToolStatusPending || slices.Contains(toolCallIDs, msg.ToolCall.ID) {
			continue
		}
		animation.StopView(m.views[i])
		m.messages = slices.Delete(m.messages, i, i+1)
		m.views = slices.Delete(m.views, i, i+1)
		m.invalidateAllItems()
	}
	m.committedMessageCount = len(m.messages)
	return cmd
}

func (m *model) AppendToLastMessage(agentName, content string) tea.Cmd {
	m.removeSpinner()

	// The first assistant chunk replaces the pending-response spinner. After
	// removal the transcript can legitimately be empty; create the streaming
	// message rather than dropping the first and every later chunk.
	if len(m.messages) == 0 {
		return m.addMessage(types.Agent(types.MessageTypeAssistant, agentName, content))
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	// Append to an existing post-snapshot assistant message from the same agent.
	if lastIdx >= max(m.loadedMessageCount, m.committedMessageCount) && lastMsg.Type == types.MessageTypeAssistant && lastMsg.InputOrigin != session.InputOriginAgent && lastMsg.Sender == agentName {
		if m.userHasScrolled {
			if len(m.deferredTail) == 0 {
				m.deferredTailIndex = lastIdx
			}
			m.deferredTail = append(m.deferredTail, content)
			return nil
		}
		materializeCmd := m.materializeDeferredTail()
		cmd := m.views[lastIdx].(message.Model).AppendContent(content)
		m.refreshRenderedItem(lastIdx)
		m.visualGeneration++
		return tea.Batch(materializeCmd, cmd)
	}

	return m.addMessage(types.Agent(types.MessageTypeAssistant, agentName, content))
}

// AppendAssistantMedia mirrors AppendToLastMessage for generated media: it
// replaces a pending spinner and joins the agent's current assistant
// message so the media renders inside the same turn as the streamed text,
// or starts a media-only assistant message when there is none.
func (m *model) AppendAssistantMedia(agentName string, media []types.AssistantMedia) tea.Cmd {
	if len(media) == 0 {
		return nil
	}
	m.removeSpinner()

	if len(m.messages) > 0 {
		lastIdx := len(m.messages) - 1
		lastMsg := m.messages[lastIdx]
		if lastMsg.Type == types.MessageTypeAssistant && lastMsg.InputOrigin != session.InputOriginAgent && lastMsg.Sender == agentName {
			lastMsg.AssistantMedia = append(lastMsg.AssistantMedia, media...)
			cmd := m.views[lastIdx].(message.Model).SetMessage(lastMsg)
			m.invalidateItem(lastIdx)
			return cmd
		}
	}

	msg := types.Agent(types.MessageTypeAssistant, agentName, "")
	msg.AssistantMedia = media
	return m.addMessage(msg)
}

// UpdateAssistantMedia replaces attached media items in place by ID. See
// Model.UpdateAssistantMedia.
func (m *model) UpdateAssistantMedia(media []types.AssistantMedia) tea.Cmd {
	byID := make(map[uint64]types.AssistantMedia, len(media))
	for _, item := range media {
		if item.ID != 0 {
			byID[item.ID] = item
		}
	}
	if len(byID) == 0 {
		return nil
	}

	var cmds []tea.Cmd
	for i, msg := range m.messages {
		changed := false
		for j, item := range msg.AssistantMedia {
			if resolved, ok := byID[item.ID]; ok {
				msg.AssistantMedia[j] = resolved
				changed = true
			}
		}
		if !changed {
			continue
		}
		if view, ok := m.views[i].(message.Model); ok {
			if cmd := view.SetMessage(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		m.invalidateItem(i)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *model) AppendReasoning(agentName, content string) tea.Cmd {
	m.removeSpinner()

	if len(m.messages) == 0 {
		return m.addReasoningBlock(agentName, content)
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	// Append to existing reasoning block for this agent — but never merge
	// into a snapshot-rendered block (streamed content is always a new
	// message; see AppendToLastMessage).
	if lastIdx >= max(m.loadedMessageCount, m.committedMessageCount) && lastMsg.Type == types.MessageTypeAssistantReasoningBlock && lastMsg.Sender == agentName {
		if block, ok := m.views[lastIdx].(*reasoningblock.Model); ok {
			block.AppendReasoning(content)
			lastMsg.Content += content // Keep content in sync for copying
			m.invalidateItem(lastIdx)
			return nil
		}
	}

	// Create a new reasoning block
	return m.addReasoningBlock(agentName, content)
}

// lastMessage returns the most recently appended message, or nil when empty.
func (m *model) lastMessage() *types.Message {
	if len(m.messages) == 0 {
		return nil
	}
	return m.messages[len(m.messages)-1]
}

// showReasoningAgentBadge reports whether a new reasoning block for agentName
// should render its agent badge. Mirrors message.sameAgentAsPrevious: the
// badge is hidden only when the block visually continues content from the
// same agent (assistant text, reasoning, or tool activity). In particular, a
// transfer_task tool call is sent by the parent agent, so reasoning from the
// sub-agent that follows it shows the sub-agent's badge.
func showReasoningAgentBadge(previous *types.Message, agentName string) bool {
	if previous == nil || previous.Sender != agentName {
		return true
	}
	switch previous.Type {
	case types.MessageTypeAssistant,
		types.MessageTypeAssistantReasoningBlock,
		types.MessageTypeToolCall,
		types.MessageTypeToolResult:
		return false
	default:
		return true
	}
}

// addReasoningBlock creates a new reasoning block message.
//
// Reasoning blocks routinely interleave with an actively streaming assistant
// turn (the LLM emits a thought, then resumes content). Finalizing the
// previous view here would drop the renderCache and IncrementalRenderer of
// a message the user is still watching, and the very next chunk would have
// to rebuild them from scratch via the transient renderer path. The next
// non-reasoning entry (user message, tool call, error) will finalize via
// addMessage when the streaming turn actually ends.
func (m *model) addReasoningBlock(agentName, content string) tea.Cmd {
	m.clearSelection()
	shouldAutoScroll := !m.userHasScrolled

	msg := &types.Message{
		Type:    types.MessageTypeAssistantReasoningBlock,
		Sender:  agentName,
		Content: content,
	}

	block := reasoningblock.New(m.ar, nextBlockID(), agentName, m.sessionState)
	block.SetShowAgentBadge(showReasoningAgentBadge(m.lastMessage(), agentName))
	block.SetReasoning(content)
	block.SetSize(m.contentWidth(), 0)

	// Same tail-spinner pinning as addMessage.
	if tail := len(m.messages) - 1; tail >= 0 && m.messages[tail].Type == types.MessageTypeSpinner {
		m.messages = slices.Insert(m.messages, tail, msg)
		m.views = slices.Insert(m.views, tail, layout.Model(block))
	} else {
		m.messages = append(m.messages, msg)
		m.views = append(m.views, block)
	}
	m.sessionState.SetPreviousMessage(msg)
	m.renderDirty = true

	var cmds []tea.Cmd
	if initCmd := block.Init(); initCmd != nil {
		cmds = append(cmds, initCmd)
	}
	if shouldAutoScroll {
		cmds = append(cmds, func() tea.Msg {
			return scrollToBottomMsg{}
		})
	}

	return tea.Batch(cmds...)
}

// getActiveReasoningBlock returns the active reasoning block for the given agent,
// or nil if the last message is not a reasoning block for that agent.
func (m *model) getActiveReasoningBlock(agentName string) (*reasoningblock.Model, int) {
	if len(m.messages) == 0 {
		return nil, -1
	}

	lastIdx := len(m.messages) - 1
	lastMsg := m.messages[lastIdx]

	if lastMsg.Type == types.MessageTypeAssistantReasoningBlock && lastMsg.Sender == agentName {
		if block, ok := m.views[lastIdx].(*reasoningblock.Model); ok {
			return block, lastIdx
		}
	}

	return nil, -1
}

func (m *model) ScrollToBottom() tea.Cmd {
	return func() tea.Msg {
		return scrollToBottomMsg{}
	}
}

func (m *model) AdjustBottomSlack(delta int) {
	if delta == 0 {
		return
	}
	m.bottomSlack = max(0, min(m.bottomSlack+delta, m.maxBottomSlack()))
}

// contentWidth returns the width available for content.
// Always reserves space for scrollbar (gap + bar) to prevent layout shifts.
func (m *model) contentWidth() int {
	return m.scrollview.ContentWidth()
}

func (m *model) totalScrollableHeight() int {
	return m.totalHeight + m.bottomSlack
}

// Helper methods
func (m *model) createToolCallView(msg *types.Message) layout.Model {
	view := tool.New(m.ar, msg, m.sessionState, m.subagents)
	view.SetSize(m.contentWidth(), 0)
	return view
}

func (m *model) createMessageView(msg *types.Message) layout.Model {
	if !lifecycle.IsUserInput(msg.InputOrigin) {
		msg.InputReference = m.resolveInputReference(msg)
	}
	view := message.New(m.ar, msg, m.sessionState.PreviousMessage())
	view.SetSize(m.contentWidth(), 0)
	return view
}

func (m *model) RemoveSpinner() {
	m.removeSpinner()
}

// MessageTypeCount returns how many messages currently in the list have the
// given type, by scanning the real message slice — never a call counter.
func (m *model) MessageTypeCount(t types.MessageType) int {
	count := 0
	for _, msg := range m.messages {
		if msg.Type == t {
			count++
		}
	}
	return count
}

func (m *model) removeSpinner() {
	if len(m.messages) == 0 {
		return
	}

	lastIdx := len(m.messages) - 1
	if m.messages[lastIdx].Type == types.MessageTypeSpinner {
		// Stop any animation subscriptions before removing the view
		if lastIdx < len(m.views) {
			animation.StopView(m.views[lastIdx])
			m.views = m.views[:lastIdx]
		}
		m.messages = m.messages[:lastIdx]
		// The removed spinner owns no cached payload. Keep historical ranges
		// valid until the next rejoin, including when history exceeds 500 items.
		delete(m.renderedItems, lastIdx)
		m.urlSpans.clear()
		m.renderDirty = true
		m.invalidateView()
		return
	}

	// Defensive fallback: a spinner buried mid-list (the tail invariant was
	// broken by a direct append) would otherwise be stuck forever. Rare path,
	// so the full cache invalidation the index shift requires is acceptable.
	for i := range slices.Backward(m.messages) {
		if m.messages[i].Type != types.MessageTypeSpinner {
			continue
		}
		if i < len(m.views) {
			animation.StopView(m.views[i])
			m.views = slices.Delete(m.views, i, i+1)
		}
		m.messages = slices.Delete(m.messages, i, i+1)
		if m.selectedMessageIndex > i {
			m.selectedMessageIndex--
		}
		if m.hoveredMessageIndex > i {
			m.hoveredMessageIndex--
		}
		m.invalidateAllItems()
		return
	}
}

func (m *model) removePendingToolCallMessages() {
	toolCallMessages := make([]*types.Message, 0, len(m.messages))
	views := make([]layout.Model, 0, len(m.views))

	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeToolCall &&
			(msg.ToolStatus == types.ToolStatusPending || msg.ToolStatus == types.ToolStatusRunning) {
			// Stop any animation subscriptions before removing the view
			if i < len(m.views) {
				animation.StopView(m.views[i])
			}
			continue
		}

		toolCallMessages = append(toolCallMessages, msg)
		if i < len(m.views) {
			views = append(views, m.views[i])
		}
	}

	if len(toolCallMessages) != len(m.messages) {
		m.messages = toolCallMessages
		m.views = views
		m.invalidateAllItems()
	}
}

// stopReasoningBlockAnimations stops spinner animations in reasoning blocks
// that have in-progress tool calls. Called on stream cancellation to prevent
// spinners from running indefinitely after ESC is pressed.
func (m *model) stopReasoningBlockAnimations() {
	for i, msg := range m.messages {
		if msg.Type != types.MessageTypeAssistantReasoningBlock || i >= len(m.views) {
			continue
		}
		block, ok := m.views[i].(*reasoningblock.Model)
		if !ok {
			continue
		}
		block.StopAnimation()
		m.invalidateItem(i)
	}
}

// labelHit reports whether a click at (localLine, col) lands on the given
// label within the rendered lines of message msgIdx. It matches the label
// text in the ANSI-stripped rendered line, so callers must gate on the state
// that makes the label visible (hover, selection, message type).
func (m *model) labelHit(msgIdx, localLine, col int, label string) bool {
	if msgIdx < 0 || msgIdx >= len(m.messages) || msgIdx >= len(m.views) {
		return false
	}

	item := m.renderItem(msgIdx, m.views[msgIdx])
	var lines []string
	if item.segments != nil {
		lines = append(lines, item.segments.Header...)
		lines = append(lines, item.segments.Stable...)
		lines = append(lines, item.segments.Tail...)
	} else {
		lines = item.lines
	}
	if localLine < 0 || localLine >= len(lines) {
		return false
	}

	plainLine := ansi.Strip(lines[localLine])
	before, _, ok := strings.Cut(plainLine, label)
	if !ok {
		return false
	}

	labelStart := ansi.StringWidth(before)
	return col >= labelStart && col < labelStart+ansi.StringWidth(label)
}

// isActionRowVisible reports whether the hover-action labels (edit, copy)
// are currently rendered for the message: under the mouse, or keyboard-selected.
func (m *model) isActionRowVisible(msgIdx int) bool {
	return msgIdx == m.hoveredMessageIndex || (m.focused && msgIdx == m.selectedMessageIndex)
}

// isEditLabelClick checks if the click is on the edit label of a user message.
// The label lives on the action row — the first line of the user bubble — so
// the hit test is pinned to localLine 0: message content that happens to
// contain the label text can never become a click target.
func (m *model) isEditLabelClick(msgIdx, localLine, col int) bool {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return false
	}
	msg := m.messages[msgIdx]
	if msg.Type != types.MessageTypeUser || msg.SessionPosition == nil {
		return false
	}
	if localLine != 0 || !m.isActionRowVisible(msgIdx) {
		return false
	}
	return m.labelHit(msgIdx, localLine, col, types.UserMessageEditLabel)
}

func (m *model) renderedItemLines(item renderedItem) []string {
	if item.segments == nil {
		return item.lines
	}
	lines := make([]string, 0, item.height)
	lines = append(lines, item.segments.Header...)
	lines = append(lines, item.segments.Stable...)
	lines = append(lines, item.segments.Tail...)
	return lines
}

// codeBlockAt returns the raw code of the fenced code block whose copy label
// is at the given click position, if any.
func (m *model) codeBlockAt(msgIdx, localLine, col int) (string, bool) {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return "", false
	}
	if msgIdx >= len(m.views) {
		return "", false
	}
	mv, ok := m.views[msgIdx].(message.Model)
	if !ok {
		return "", false
	}
	blocks := mv.CodeBlocks()
	if len(blocks) == 0 {
		return "", false
	}
	var target *markdown.CodeBlock
	for i := range blocks {
		if blocks[i].Line == localLine {
			target = &blocks[i]
			break
		}
	}
	if target == nil {
		return "", false
	}

	item := m.renderItem(msgIdx, m.views[msgIdx])
	lines := m.renderedItemLines(item)
	if localLine < 0 || localLine >= len(lines) {
		return "", false
	}
	plainLine := ansi.Strip(lines[localLine])
	before, _, found := strings.Cut(plainLine, markdown.CodeBlockCopyIcon)
	if !found {
		return "", false
	}
	iconStart := ansi.StringWidth(before)
	iconEnd := iconStart + ansi.StringWidth(markdown.CodeBlockCopyIcon)
	if col < iconStart || col >= iconEnd {
		return "", false
	}
	return target.Content, true
}

// isCopyLabelClick checks if the click is on the copy label of a message.
func (m *model) isCopyLabelClick(msgIdx, localLine, col int) bool {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return false
	}
	if !m.isActionRowVisible(msgIdx) {
		return false
	}
	switch m.messages[msgIdx].Type {
	case types.MessageTypeUser:
		// The action row is the first line of the user bubble; pinning the
		// hit test there rules out collisions with message content.
		if localLine != 0 {
			return false
		}
	case types.MessageTypeAgentInput, types.MessageTypeRuntimeNotice:
		view, ok := m.views[msgIdx].(interface{ CopyActionLine() int })
		if !ok || localLine != view.CopyActionLine() {
			return false
		}
	case types.MessageTypeAssistant:
	default:
		return false
	}
	return m.labelHit(msgIdx, localLine, col, types.MessageCopyLabel)
}

// isRetryLabelClick checks if the click is on the retry label of an error message.
func (m *model) isRetryLabelClick(msgIdx, localLine, col int) bool {
	if msgIdx < 0 || msgIdx >= len(m.messages) || m.messages[msgIdx].Type != types.MessageTypeError {
		return false
	}
	return m.labelHit(msgIdx, localLine, col, types.ErrorRetryLabel)
}

// copyMessageToClipboard copies the content of a specific message to clipboard.
// Silent: the caller flashes an inline "copied" confirmation on the clicked label.
func (m *model) copyMessageToClipboard(msgIdx int) tea.Cmd {
	if msgIdx < 0 || msgIdx >= len(m.messages) {
		return nil
	}
	content := copyableMessageContent(m.messages[msgIdx])
	if content == "" {
		return nil
	}
	return copyTextToClipboardSilent(content)
}

func (m *model) mouseToLineCol(x, y int) (line, col int) {
	adjustedX := max(0, x-m.xPos)
	adjustedY := max(0, y-m.yPos)
	return m.scrollOffset + adjustedY, adjustedX
}

// SubagentNodeAt returns the subagent node id referenced by the subagent tool
// message at the given screen coordinates ("Spawned x (id)" and friends), so
// a click on it can open a tab attached to that subagent.
func (m *model) SubagentNodeAt(x, y int) (subagent.NodeID, bool) {
	ref, ok := m.InputReferenceAt(x, y)
	return subagent.NodeID(ref.ID), ok && ref.Kind == lifecycle.InputReferenceNode
}

func (m *model) isMouseOnScrollbar(x, y int) bool {
	if m.totalHeight <= m.height {
		return false
	}
	return x == m.scrollview.ScrollbarX() && y >= m.yPos && y < m.yPos+m.height
}

func (m *model) IsScrollbarDragging() bool {
	return m.scrollview.IsDragging()
}

// CancelScrollbarDrag releases only pointer capture, preserving the viewport,
// selection, pending copy identity and transcript caches.
func (m *model) CancelScrollbarDrag() {
	if m.scrollview.IsDragging() {
		_, _ = m.scrollview.UpdateMouse(tea.MouseReleaseMsg{Button: tea.MouseLeft})
		m.visualGeneration++
	}
}

// IsSelecting returns true while a text-selection drag is in progress. The
// app-level mouse routing uses it to keep delivering motion and release
// events to this component even when the cursor leaves the chat region.
func (m *model) IsSelecting() bool {
	return m.selection.mouseButtonDown && m.selection.active
}

func (m *model) IsMouseOnScrollbar(x, y int) bool {
	return m.isMouseOnScrollbar(x, y)
}

func (m *model) handleScrollviewUpdate(msg tea.Msg) (layout.Model, tea.Cmd) {
	m.CancelReferenceHover()
	// Drag calculations depend on total height and may jump directly into the
	// stale final item, so reconcile before delegating any active drag update.
	var materializeCmd tea.Cmd
	if m.scrollview.IsDragging() {
		materializeCmd = m.materializeDeferredTailForInteraction()
	}
	_, cmd := m.scrollview.UpdateMouse(msg)
	m.scrollOffset = m.scrollview.ScrollOffset()
	if m.isAtBottom() {
		m.userHasScrolled = false
	} else {
		m.userHasScrolled = true
		m.bottomSlack = 0
	}
	return m, tea.Batch(materializeCmd, cmd)
}

// hasAnimatedContent returns true if the message list contains content that
// requires tick-driven updates (spinners, fades, etc.). Used to decide whether
// to invalidate the render cache on animation ticks.
func (m *model) hasAnimatedContent() bool {
	for i := range m.messages {
		if m.itemNeedsTick(i) {
			return true
		}
	}
	return false
}

func (m *model) itemNeedsTick(i int) bool {
	if i < len(m.views) {
		if view, ok := m.views[i].(interface{ NeedsTick() bool }); ok && view.NeedsTick() {
			return true
		}
	}
	msg := m.messages[i]
	switch msg.Type {
	case types.MessageTypeSpinner, types.MessageTypeLoading:
		return true
	case types.MessageTypeToolCall:
		return msg.ToolStatus == types.ToolStatusPending || msg.ToolStatus == types.ToolStatusRunning
	case types.MessageTypeAssistantReasoningBlock:
		if i < len(m.views) {
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				return block.NeedsTick()
			}
		}
	}
	return false
}

// StartInlineEdit begins inline editing for the specified message.
func (m *model) StartInlineEdit(msgIndex, sessionPosition int, content string) tea.Cmd {
	if msgIndex < 0 || msgIndex >= len(m.messages) {
		return nil
	}

	msg := m.messages[msgIndex]
	if msg.Type != types.MessageTypeUser {
		return nil
	}

	// Save the current selection state before entering inline edit
	// This allows restoring when the edit is cancelled
	m.inlineEditPrevSelection = m.selectedMessageIndex

	// Set focused state but clear any message selection to prevent highlight
	m.focused = true
	m.selectedMessageIndex = -1

	m.inlineEditMsgIndex = msgIndex
	m.inlineEditSessionPos = sessionPosition
	m.inlineEditOriginal = content

	// Create and configure the textarea
	ta := textarea.New()
	ta.SetValue(content)
	ta.Focus()

	// Configure appearance - use a style similar to user message
	innerWidth := m.contentWidth() - styles.UserMessageStyle.GetHorizontalFrameSize()
	if innerWidth > 0 {
		ta.SetWidth(innerWidth)
	}

	// Set a generous height so the textarea's internal viewport never scrolls.
	// This prevents cursor positioning bugs with multi-line content. The actual
	// rendered output is trimmed in renderInlineEditTextarea to remove padding.
	ta.SetHeight(max(1, m.height))

	// Remove the default prompt/placeholder styling for a cleaner look
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // No limit

	ta.SetStyles(inlineEditStyles())

	// Share the composer resolution, including configured Shift+Enter ownership.
	enhanced := m.keyboardEnhancementsSupported || termfeatures.SupportsModifiedEnter(os.Getenv)
	ta.KeyMap.InsertNewline.SetKeys(core.EditorNewlineKeys(enhanced)...)
	ta.KeyMap.InsertNewline.SetEnabled(true)

	m.inlineEditTextarea = ta
	m.invalidateItem(msgIndex)
	m.renderDirty = true

	return ta.Focus()
}

func inlineEditStyles() textarea.Styles {
	return textarea.Styles{
		Focused: textarea.StyleState{
			Base:        styles.BaseStyle.Background(styles.BackgroundAlt),
			Placeholder: styles.BaseStyle.Background(styles.BackgroundAlt).Foreground(styles.PlaceholderColor),
		},
		Blurred: textarea.StyleState{
			Base:        styles.BaseStyle.Background(styles.BackgroundAlt),
			Placeholder: styles.BaseStyle.Background(styles.BackgroundAlt).Foreground(styles.PlaceholderColor),
		},
		Cursor: textarea.CursorStyle{
			Color: styles.Accent,
		},
	}
}

// CancelInlineEdit cancels the current inline edit and restores the original content.
func (m *model) CancelInlineEdit() tea.Cmd {
	if m.inlineEditMsgIndex < 0 {
		return nil
	}

	msgIndex := m.inlineEditMsgIndex
	prevSelection := m.inlineEditPrevSelection

	m.inlineEditMsgIndex = -1
	m.inlineEditSessionPos = -1
	m.inlineEditOriginal = ""
	m.inlineEditTextarea = textarea.Model{}
	m.inlineEditPrevSelection = -1

	// Restore the previous selection state if we were in keyboard selection mode
	if prevSelection >= 0 {
		m.selectedMessageIndex = prevSelection
		m.focused = true
	} else {
		// We weren't in selection mode, blur the messages component
		m.focused = false
		m.selectedMessageIndex = -1
	}

	m.invalidateItem(msgIndex)
	m.invalidateAllItems() // Invalidate all to update selection highlight
	m.renderDirty = true

	return core.CmdHandler(InlineEditCancelledMsg{WasInSelectionMode: prevSelection >= 0})
}

// IsInlineEditing returns true if inline editing is currently active.
func (m *model) IsInlineEditing() bool {
	return m.inlineEditMsgIndex >= 0
}

// InlineEditCancelledMsg is sent when inline editing is cancelled.
type InlineEditCancelledMsg struct {
	WasInSelectionMode bool // True if we were in keyboard selection mode before editing
}

// commitInlineEdit commits the inline edit and sends the message.
func (m *model) commitInlineEdit() tea.Cmd {
	if m.inlineEditMsgIndex < 0 {
		return nil
	}

	content := strings.TrimSpace(m.inlineEditTextarea.Value())
	sessionPos := m.inlineEditSessionPos

	// Reset editing state
	m.inlineEditMsgIndex = -1
	m.inlineEditSessionPos = -1
	m.inlineEditOriginal = ""
	m.inlineEditTextarea = textarea.Model{}

	m.invalidateAllItems()

	if content == "" {
		// Empty content is treated as cancellation - notify the chat page
		return core.CmdHandler(InlineEditCancelledMsg{})
	}

	// Emit InlineEditCommittedMsg with the edited content - the chat page handles branching
	return core.CmdHandler(InlineEditCommittedMsg{
		SessionPosition: sessionPos,
		Content:         content,
	})
}

// InlineEditCommittedMsg is sent when inline editing is committed.
type InlineEditCommittedMsg struct {
	SessionPosition int
	Content         string
}

func (m *model) StopAnimations() {
	m.CancelReferenceHover()
	m.cancelImageClick()
	m.slackAnimationSub.Stop()
	for _, v := range m.views {
		animation.StopView(v)
	}
}

// ResumeAnimations reacquires only presentation subscriptions. It does not
// initialize views, retry image loading, rebuild markdown, or change scrolling.
func (m *model) ResumeAnimations() tea.Cmd {
	var cmds []tea.Cmd
	if m.bottomSlack > 0 {
		cmds = append(cmds, m.slackAnimationSub.Start())
	}
	for i, view := range m.views {
		// Hidden time may finish a reasoning fade. Refresh that live item only,
		// not the historical transcript or its markdown caches.
		if block, ok := view.(*reasoningblock.Model); ok && block.NeedsTick() {
			m.invalidateItem(i)
		}
		if owner, ok := view.(interface{ ResumeAnimation() tea.Cmd }); ok {
			cmds = append(cmds, owner.ResumeAnimation())
		}
	}
	m.visualGeneration++
	return tea.Batch(cmds...)
}

func (m *model) RemovePendingSessionPosition(position int) {
	if position < 0 {
		return
	}
	for _, msg := range m.messages {
		if msg.SessionPosition != nil && *msg.SessionPosition > position {
			*msg.SessionPosition--
		}
	}
	if position < m.loadedItemCount {
		m.loadedItemCount--
	}
}

func copyableMessageContent(msg *types.Message) string {
	if msg.IsSubagentReply() {
		return msg.ReceivedBody
	}
	return msg.Content
}
