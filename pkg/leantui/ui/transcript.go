package ui

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

type blockKind int

type PendingUserKind int

const (
	blockReasoning blockKind = iota
	blockAssistant
)

const (
	PendingUserSteer PendingUserKind = iota
	PendingUserFollowUp
)

type PendingUserMessage struct {
	ID      string
	Display string
	Content string
	TurnID  string
	Kind    PendingUserKind
}

// pendingBlock accumulates the text of the block currently being streamed.
type pendingBlock struct {
	kind blockKind
	text strings.Builder
}

// block is a finalized piece of the conversation. Its lines are rendered lazily
// and cached per width, so finalized content is not re-rendered every frame and
// only reflows when the terminal is resized.
type block struct {
	render func(width int) []string
	cacheW int
	cache  []string
	cached bool
	input  bool
}

func (b *block) lines(width int) []string {
	if !b.cached || b.cacheW != width {
		b.cache = b.render(width)
		b.cacheW = width
		b.cached = true
	}
	return b.cache
}

// Transcript owns everything that scrolls: the finalized conversation blocks,
// the in-progress streamed block, and the in-flight tool calls. Committed
// blocks are immutable scrollback; the pending block and tool calls are the
// live region that changes each frame until they finalize into blocks.
type Transcript struct {
	blocks  []*block
	pending *pendingBlock
	toolz   *ToolTracker
}

// NewTranscript creates an empty transcript.
func NewTranscript() *Transcript {
	return &Transcript{toolz: NewToolTracker()}
}

func (t *Transcript) Clear() {
	t.blocks = nil
	t.ClearActive()
}

// ClearActive drops the live region (the streamed block and any in-flight tool
// calls) while keeping the committed scrollback intact. Used when starting a
// new session.
func (t *Transcript) ClearActive() {
	t.pending = nil
	t.toolz.Reset()
}

func (t *Transcript) AddUser(content string) {
	t.AddBlock(func(w int) []string { return RenderUserLines(content, w) })
}

func (t *Transcript) AddAssistant(content string) {
	t.AddBlock(func(w int) []string { return RenderAssistantLines(content, w) })
}

// AddBlock appends a finalized, lazily-rendered block to the conversation.
func (t *Transcript) AddBlock(render func(width int) []string) {
	t.blocks = append(t.blocks, &block{render: render})
}

func (t *Transcript) appendPending(kind blockKind, content string) {
	if content == "" {
		return
	}
	if t.pending == nil || t.pending.kind != kind {
		t.FlushPending()
		t.pending = &pendingBlock{kind: kind}
	}
	t.pending.text.WriteString(content)
}

// AppendReasoning appends streamed reasoning text.
func (t *Transcript) AppendReasoning(content string) { t.appendPending(blockReasoning, content) }

// AppendAssistant appends streamed assistant text.
func (t *Transcript) AppendAssistant(content string) { t.appendPending(blockAssistant, content) }

// FlushPending finalizes the in-progress streamed block into the conversation.
func (t *Transcript) FlushPending() {
	if t.pending == nil {
		return
	}
	text := t.pending.text.String()
	kind := t.pending.kind
	t.pending = nil

	switch kind {
	case blockReasoning:
		t.AddBlock(func(w int) []string { return RenderReasoningLines(text, w) })
	case blockAssistant:
		t.AddBlock(func(w int) []string { return RenderAssistantLines(text, w) })
	}
}

// CommitAssistant fences text and discards only orphan, unpublished tool fragments.
func (t *Transcript) CommitAssistant(agentName string, committed []string) {
	t.FlushPending()
	var orphaned []string
	t.toolz.ForEach(func(view *ToolView) {
		msg := view.Message()
		if msg != nil && msg.Sender == agentName && msg.ToolStatus == tuitypes.ToolStatusPending && !slices.Contains(committed, msg.ToolCall.ID) {
			orphaned = append(orphaned, msg.ToolCall.ID)
		}
	})
	for _, id := range orphaned {
		t.toolz.Remove(id)
	}
}

// UpsertTool creates or updates an in-flight tool call.
func (t *Transcript) UpsertTool(agentName string, toolCall tools.ToolCall, toolDef tools.Tool, status tuitypes.ToolStatus) {
	t.toolz.Upsert(agentName, toolCall, toolDef, status)
}

// Tool returns an in-flight tool call by id.
func (t *Transcript) Tool(id string) *ToolView { return t.toolz.Get(id) }

// RemoveTool removes an in-flight tool call by id.
func (t *Transcript) RemoveTool(id string) { t.toolz.Remove(id) }

// FinishTool commits a completed tool call as an immutable block.
func (t *Transcript) FinishTool(id string, result ToolResult, sessionState service.SessionStateReader) {
	view := t.toolz.Finish(id, result)
	if view == nil {
		return
	}
	t.AddBlock(func(w int) []string { return RenderToolWithState(view, w, 0, sessionState) })
}

// Lines renders everything that scrolls: finalized blocks, the in-progress
// streamed block, running tool calls, and user messages waiting to be accepted
// by the runtime. A blank line separates each entry. Activity belongs to the
// session-local composer header, never to the scrolling transcript.
func (t *Transcript) Lines(width, spinnerFrame int, busy bool, sessionState service.SessionStateReader, pendingUsers []PendingUserMessage) []string {
	var lines []string
	for _, b := range t.blocks {
		lines = append(lines, b.lines(width)...)
		lines = append(lines, "")
	}
	if t.pending != nil {
		lines = append(lines, t.pendingLines(width)...)
		lines = append(lines, "")
	}
	t.toolz.ForEach(func(tv *ToolView) {
		lines = append(lines, RenderToolWithState(tv, width, spinnerFrame, sessionState)...)
		lines = append(lines, "")
	})
	for _, msg := range pendingUsers {
		lines = append(lines, RenderPendingUserLines(msg, width)...)
		lines = append(lines, "")
	}
	return lines
}

// BlockCount reports the number of committed transcript blocks.
func (t *Transcript) BlockCount() int { return len(t.blocks) }

// BlockLines renders a committed block by index.
func (t *Transcript) BlockLines(index, width int) []string {
	if index < 0 || index >= len(t.blocks) {
		return nil
	}
	return t.blocks[index].lines(width)
}

// ToolCount reports the number of in-flight tool calls.
func (t *Transcript) ToolCount() int { return t.toolz.Len() }

// ToolByIDCount reports the number of tracked tool-call ids.
func (t *Transcript) ToolByIDCount() int { return t.toolz.ByIDLen() }

// pendingLines renders the message currently being streamed. Assistant text is
// rendered as markdown live (the same renderer used once it is finalized), so
// formatting appears as it streams.
func (t *Transcript) pendingLines(width int) []string {
	text := t.pending.text.String()
	switch t.pending.kind {
	case blockReasoning:
		return RenderReasoningLines(text, width)
	case blockAssistant:
		return RenderAssistantLines(text, width)
	default:
		return nil
	}
}

func (t *Transcript) AddInputBlock(render func(int) []string) {
	t.blocks = append(t.blocks, &block{render: render, input: true})
}

func (t *Transcript) InvalidateInputBlocks() {
	for _, b := range t.blocks {
		if b.input {
			b.cached = false
		}
	}
}

// InvalidateCaches re-renders presentation after shared preferences/theme change
// without replacing the transcript, pending blocks, tool state, or draft.
func (t *Transcript) InvalidateCaches() {
	for _, block := range t.blocks {
		block.cached = false
	}
}
