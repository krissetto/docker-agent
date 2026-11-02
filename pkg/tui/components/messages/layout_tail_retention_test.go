package messages

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// tailRetentionProse wraps across several rows, so a live tail owns more than one row.
const tailRetentionProse = "streamed words that wrap across several rows of the viewport so the live tail owns more than one line of text"

// tailRetentionStartHandoff starts one real reply handoff through the production append
// path and stops while it is active (the first prose chunk has started it).
func tailRetentionStartHandoff(t *testing.T, m *model) {
	t.Helper()
	m.suppressReplyHandoff = true
	for i := range 3 {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("tail seed %d prose", i)))
		m.committedMessageCount = len(m.messages)
	}
	m.suppressReplyHandoff = false
	_ = m.View()
	call := tools.ToolCall{ID: "tail-retention-call", Function: tools.FunctionCall{Name: "check"}}
	_ = m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantMedia("root", []types.AssistantMedia{{Fallback: "tail retention media"}})
	_ = m.AddAssistantMessage("root", "")
	_ = m.View()
	require.False(t, m.renderDirty, "precondition: rendered before the first prose chunk")
	_ = m.AppendToLastMessage("root", "tail retention prose")
	require.True(t, m.ReplyHandoffActive(), "the first prose chunk must start a reply handoff")
	require.NotEmpty(t, fourthReplyRetiringGroups(m), "the oldest visible reply must retire")
	require.NotEmpty(t, m.replyHandoff.Rows(), "the handoff must own rows")
}

// tailRetentionRetire drives accepted animation ticks until the handoff retires.
func tailRetentionRetire(t *testing.T, m *model) {
	t.Helper()
	for range 500 {
		cmd := m.ar.Continue()
		if cmd == nil {
			break
		}
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok, "tick must be accepted")
		_, _ = m.Update(tick)
		if !m.ReplyHandoffActive() {
			return
		}
	}
	require.FailNow(t, "reply handoff did not retire through accepted ticks")
}

// tailRetentionHandoffSources returns the sorted source slots the active handoff keeps.
func tailRetentionHandoffSources(m *model) []int {
	keep := m.handoffRetained()
	out := make([]int, 0, len(keep))
	for s := range keep {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// TestTailRetentionActiveHandoffSourcesKeptUntilRetired: the rows of an active reply
// handoff stay materialized at the top, the bottom and across repeated passes (no
// re-render), and after retirement exactly the sources outside the retention window are
// pruned.
func TestTailRetentionActiveHandoffSourcesKeptUntilRetired(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	tailRetentionStartHandoff(t, m)
	// The frame rows the retention reads are synced by the first View after the handoff starts.
	_ = m.View()
	// Only rows that read a source message own that source's body: composite and work
	// header rows have no source, so they do not keep a body.
	rowSeen := map[int]bool{}
	for _, r := range m.replyHandoff.Rows() {
		if r.Source == nil {
			continue
		}
		if idx := m.messagePointerIndex(r.Source); idx >= 0 {
			rowSeen[idx] = true
		}
	}
	rowSources := make([]int, 0, len(rowSeen))
	for s := range rowSeen {
		rowSources = append(rowSources, s)
	}
	slices.Sort(rowSources)
	sources := tailRetentionHandoffSources(m)
	require.NotEmpty(t, sources, "the active handoff keeps source slots")
	require.Equal(t, rowSources, sources, "the kept sources are exactly the handoff rows' sources")

	// The reader at the end is pinned: the handoff sources are materialized.
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.ReplyHandoffActive(), "still active while pinned")
	pinnedWindow, _, _ := vprlRetentionWindow(m)
	for _, s := range sources {
		require.Truef(t, m.hiddenSlot(s) || m.materialized(s), "pinned: active handoff source %d stays materialized (hidden=%v inWindow=%v)", s, m.hiddenSlot(s), pinnedWindow[s])
	}
	pinnedVisits := m.stats.itemVisits
	for range 3 {
		_ = m.View()
	}
	require.Equal(t, pinnedVisits, m.stats.itemVisits, "pinned: repeated passes re-render nothing")

	// The reader at the top: the handoff sources stay materialized and repeated passes
	// measure nothing.
	vprlScrollToRow(t, m, 0)
	require.True(t, m.ReplyHandoffActive(), "still active at the top")
	topWindow, _, _ := vprlRetentionWindow(m)
	bodies := 0
	for _, s := range sources {
		require.Truef(t, m.hiddenSlot(s) || m.materialized(s), "top: active handoff source %d stays materialized (hidden=%v inWindow=%v)", s, m.hiddenSlot(s), topWindow[s])
		if !m.hiddenSlot(s) {
			bodies++
		}
	}
	require.NotZero(t, bodies, "the handoff keeps at least one source body, not only work children")
	topVisits := m.stats.itemVisits
	for range 3 {
		_ = m.View()
	}
	require.Equal(t, topVisits, m.stats.itemVisits, "top: repeated passes re-render nothing")

	tailRetentionRetire(t, m)
	runtime.GC()
	vprlScrollToRow(t, m, 0)
	window, _, _ := vprlRetentionWindow(m)
	var wantPruned, gotPruned []int
	for _, s := range sources {
		if !window[s] {
			wantPruned = append(wantPruned, s)
		}
		if !m.materialized(s) {
			gotPruned = append(gotPruned, s)
		}
		if m.materialized(s) {
			require.Truef(t, window[s], "retired source %d is materialized only inside the window", s)
		}
	}
	require.NotEmpty(t, wantPruned, "at least one retired source lies outside the window")
	require.Equal(t, wantPruned, gotPruned, "after retirement exactly the sources outside the window are pruned")
}

// tailRetentionStream starts a live reply at the end of a loaded transcript, with no
// handoff, and returns the model and the live tail slot.
func tailRetentionStream(t *testing.T, prose string) (*model, int) {
	t.Helper()
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendToLastMessage("root", prose)
	_ = m.View()
	return m, len(m.views) - 1
}

// TestTailRetentionLiveTailKeptOnlyWhileStreaming covers both live states of the stream
// tail: an active segmented suffix, and a pending deferred chunk for a scrolled-away
// reader. Each stays materialized while live. After the stream finishes, a finished tail
// that is pinned and then scrolled away is pruned.
func TestTailRetentionLiveTailKeptOnlyWhileStreaming(t *testing.T) {
	t.Run("active segmented suffix", func(t *testing.T) {
		m, tail := tailRetentionStream(t, tailRetentionProse)
		require.NotNil(t, m.activeSegments, "a live stream builds its segmented suffix")
		require.True(t, m.materialized(tail))

		vprlScrollToRow(t, m, 0)
		require.NotNil(t, m.activeSegments, "the suffix stays active while the reader is away")
		require.True(t, m.materialized(tail), "the active streaming tail stays materialized while scrolled away")

		// The stream ends while the reader is pinned at the end, so the finished tail is
		// measured again in the window and then becomes ordinary offscreen history.
		_ = m.scrollToBottom()
		_ = m.View()
		require.NotNil(t, m.activeSegments, "the stream is still live before it finishes")
		require.Less(t, m.committedMessageCount, len(m.messages), "the live reply is uncommitted")
		tailRetentionComplete(m, tailRetentionProse)
		require.Equal(t, len(m.messages), m.committedMessageCount, "a finished reply is committed")
		_ = m.View()
		require.True(t, m.materialized(tail), "a pinned reader keeps the finished tail in the window")

		vprlScrollToRow(t, m, 0)
		require.False(t, m.materialized(tail), "a finished tail outside the window is pruned")
	})

	t.Run("pending deferred chunk", func(t *testing.T) {
		m, tail := tailRetentionStream(t, "ok")
		vprlScrollToRow(t, m, 0)
		require.True(t, m.userHasScrolled)

		_ = m.AppendToLastMessage("root", " deferred chunk")
		require.NotEmpty(t, m.deferredTail, "a chunk to a scrolled-away reader is deferred")
		m.layoutViewport()
		require.True(t, m.materialized(tail), "the pending deferred tail is measured and kept")
		require.NotEmpty(t, m.deferredTail, "the chunk is still pending")

		visits := m.stats.itemVisits
		for range 2 {
			m.layoutViewport()
		}
		require.Equal(t, visits, m.stats.itemVisits, "repeated passes over a pending tail re-render nothing")
		require.True(t, m.materialized(tail))

		_ = m.scrollToBottom()
		_ = m.View()
		require.True(t, m.materialized(tail), "the pinned reader sees the pending tail")
		tailRetentionComplete(m, "ok deferred chunk")
		require.Empty(t, m.deferredTail, "finalize flushes the deferred chunk")
		require.Equal(t, len(m.messages), m.committedMessageCount, "a finished reply is committed")
		_ = m.View()
		require.True(t, m.materialized(tail), "a pinned reader keeps the finished tail in the window")
		vprlScrollToRow(t, m, 0)
		require.False(t, m.materialized(tail), "a finished tail outside the window is pruned")
	})
}

// TestTailRetentionReplayedEndPrunedAtTop: a completed replayed session has its end body
// materialized by a pinned reader, and scrolling to the top prunes it. The replay commits
// every loaded message, so the end is not live.
func TestTailRetentionReplayedEndPrunedAtTop(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	require.Len(t, m.views, len(m.messages), "views and messages share one index")
	require.Equal(t, len(m.messages), m.committedMessageCount, "a replayed session is committed")
	last := len(m.views) - 1
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.materialized(last), "a pinned reader materializes the replayed end")
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(last), "the completed replayed end is pruned at the top")
	// Copy or selection of the pruned end re-materializes it on demand.
	require.NotEmpty(t, m.renderedLineAt(m.slotOffset(last)), "the pruned end renders on demand")
}

// TestTailRetentionFinalizeToCompleteKeepsFrame: finalizing the live reply (the stream's
// visual close) does not change what the pinned reader sees or the message order.
func TestTailRetentionFinalizeToCompleteKeepsFrame(t *testing.T) {
	m, tail := tailRetentionStream(t, tailRetentionProse)
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	before := m.View()
	count := len(m.messages)
	_ = m.FinalizeStreamedAssistant("root", tailRetentionProse, "", false)
	require.Len(t, m.messages, count, "finalize keeps the message count")
	require.Equal(t, tailRetentionProse, m.messages[tail].Content, "finalize keeps the streamed content")
	require.Equal(t, before, m.View(), "the pinned frame does not change at finalize")
}

// TestTailRetentionUserAndToolTailsKeepCounts: a finished reply followed by user input and
// a tool result prunes the finished reply at the top, and a new live reply after them is
// kept while it streams.
func TestTailRetentionUserAndToolTailsKeepCounts(t *testing.T) {
	m, reply := tailRetentionStream(t, tailRetentionProse)
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	_ = m.FinalizeStreamedAssistant("root", tailRetentionProse, "", false)
	_ = m.View()
	require.Equal(t, len(m.messages), m.committedMessageCount)
	_ = m.AddUserMessage("next question")
	call := tools.ToolCall{ID: "tail-counts-call", Function: tools.FunctionCall{Name: "check"}}
	_ = m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	_ = m.View()
	require.Len(t, m.views, len(m.messages), "no stale view count after user and tool messages")
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(reply), "the finished reply is pruned behind the user and tool tail")
	require.False(t, m.materialized(len(m.views)-1), "a completed tool tail is not live")

	// The reader is away from the end, so the new reply's chunk is a pending deferred chunk.
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendToLastMessage("root", "second live reply "+tailRetentionProse)
	_ = m.AppendToLastMessage("root", " and a second chunk")
	tail := len(m.views) - 1
	require.NotEmpty(t, m.deferredTail, "the new reply's chunk is deferred while the reader is away")
	require.Less(t, m.committedMessageCount, len(m.messages), "the new reply is uncommitted")
	m.layoutViewport()
	require.True(t, m.materialized(tail), "the new live reply is kept at the top")
}

// TestTailRetentionCancelledPartialPrunedAtTop: a cancelled reply has no completion
// event. Its partial stays uncommitted, but it no longer streams, so once the reader
// is away from the end it must not stay materialized.
func TestTailRetentionCancelledPartialPrunedAtTop(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantContent("cancel-session", "cancel-message", "root", tailRetentionProse)
	_ = m.View()
	tail := len(m.views) - 1
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.materialized(tail), "precondition: the live partial is materialized at the end")

	_, _ = m.Update(messages.StreamCancelledMsg{ShowMessage: false})
	_ = m.View()
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(tail), "a cancelled partial outside the window is pruned")
}

// tailRetentionComplete finishes a live reply through the production completion event.
func tailRetentionComplete(m *model, prose string) {
	msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: prose})
	_ = m.CompleteAssistant(agentruntime.MessageAddedAt("tail-session", msg, "root", 0).(*agentruntime.MessageAddedEvent))
}

// tailRetentionCompleteWithID finishes a reply of one runtime message identity.
func tailRetentionCompleteWithID(m *model, agent, messageID, prose string) {
	msg := session.NewAgentMessage(agent, &chat.Message{Role: chat.MessageRoleAssistant, Content: prose, MessageID: messageID})
	_ = m.CompleteAssistant(agentruntime.MessageAddedAt("tail-session", msg, agent, 0).(*agentruntime.MessageAddedEvent))
}

// TestTailRetentionCancelResumeCompleteThenNewStream: a cancelled partial is pruned away
// from the end; a resumed chunk pins it again; its completion releases it; a new stream
// after the completed reply is kept while the completed one stays pruned.
func TestTailRetentionCancelResumeCompleteThenNewStream(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantContent("s", "cr-1", "root", tailRetentionProse)
	_ = m.View()
	first := len(m.views) - 1
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.materialized(first), "precondition: the live reply is materialized at the end")

	_, _ = m.Update(messages.StreamCancelledMsg{ShowMessage: false})
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(first), "a cancelled partial is pruned away from the end")

	_ = m.AppendAssistantContent("s", "cr-1", "root", " resumed after cancel")
	_ = m.View()
	require.True(t, m.isLiveTail(first), "a resumed chunk pins the same reply again")
	require.True(t, m.materialized(first), "the resumed live reply is kept at the top")

	tailRetentionComplete(m, tailRetentionProse+" resumed after cancel")
	_ = m.View()
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(first), "the completed resumed reply is pruned")

	_ = m.AppendAssistantContent("s", "cr-2", "root", "second reply after cancel")
	second := len(m.views) - 1
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.materialized(second), "precondition: the new reply is materialized at the end")
	require.True(t, m.isLiveTail(second), "a new stream pins its own reply")
	vprlScrollToRow(t, m, 0)
	require.True(t, m.materialized(second), "the new live reply is kept at the top")
	require.False(t, m.materialized(first), "the completed reply stays pruned")
}

// TestTailRetentionMidStreamFlushReleasesThenChunkRepins: a flush while the stream is live
// (tests only; production calls FinalizeStream at stream end) releases the pin. The next
// non-empty chunk pins the same message again, and the content is unchanged.
func TestTailRetentionMidStreamFlushReleasesThenChunkRepins(t *testing.T) {
	m, tail := tailRetentionStream(t, tailRetentionProse)
	vprlScrollToRow(t, m, 0)
	require.True(t, m.materialized(tail), "precondition: the live tail is kept at the top")

	_ = m.FinalizeStream()
	_ = m.View()
	require.False(t, m.isLiveTail(tail), "the flush releases the cache pin")
	require.False(t, m.materialized(tail), "the released tail outside the window is pruned")

	_ = m.AppendToLastMessage("root", " and more")
	require.True(t, m.isLiveTail(tail), "the next non-empty chunk pins the same message again")
	require.Equal(t, tailRetentionProse+" and more", m.messages[tail].Content+strings.Join(m.deferredTail, ""), "the flush changes no content")
}

// TestTailRetentionReasoningTailKeptWhileLiveThenPrunedAfterStop: a reasoning chunk pins
// its block; the block is kept at the top while the stream is live, and pruned after stop.
func TestTailRetentionReasoningTailKeptWhileLiveThenPrunedAfterStop(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AppendReasoningContent("s", "r-1", "root", "thinking through the request in several sentences so the block owns more than one row of the viewport")
	_ = m.View()
	tail := len(m.views) - 1
	require.True(t, m.isLiveTail(tail), "a reasoning chunk pins its block")
	vprlScrollToRow(t, m, 0)
	require.True(t, m.materialized(tail), "a live reasoning tail is kept at the top")

	_ = m.FinalizeStream()
	_ = m.View()
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(tail), "after the stream stops the reasoning block is pruned")
}

// TestTailRetentionEmptyAndToolChunksNeverRepinCompletedTail: an empty append, an empty
// reasoning chunk, and empty appends after a user or tool message never pin, so a
// completed reply is not re-pinned by a no-op.
func TestTailRetentionEmptyAndToolChunksNeverRepinCompletedTail(t *testing.T) {
	m, tail := tailRetentionStream(t, tailRetentionProse)
	tailRetentionComplete(m, tailRetentionProse)
	require.Nil(t, m.liveTailMsg, "completion releases the pin")

	_ = m.AppendAssistantContent("s", "e-1", "root", "")
	require.Nil(t, m.liveTailMsg, "an empty append does not pin")
	_ = m.AppendReasoningContent("s", "e-1", "root", "")
	require.Nil(t, m.liveTailMsg, "an empty reasoning chunk does not pin")

	call := tools.ToolCall{ID: "empty-after-tool", Function: tools.FunctionCall{Name: "check"}}
	_ = m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	_ = m.AppendAssistantContent("s", "e-2", "root", "")
	_ = m.AppendReasoningContent("s", "e-2", "root", "")
	require.Nil(t, m.liveTailMsg, "no-op chunks after a tool message do not pin")

	_ = m.View()
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(tail), "the completed reply stays pruned after no-op chunks")
}

// TestTailRetentionUserAndToolAppendsDoNotKeepStaleTail: once a user or tool message follows
// the live reply, the old reply is no longer the last slot and is pruned at the top. The
// pin never keeps it, and a new chunk moves the pin to its own reply.
func TestTailRetentionUserAndToolAppendsDoNotKeepStaleTail(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantContent("s", "ut-1", "root", tailRetentionProse)
	_ = m.View()
	live := len(m.views) - 1
	vprlScrollToRow(t, m, 0)
	require.True(t, m.materialized(live), "precondition: the live reply is kept at the top")

	m.addMessage(types.User("user input mid-stream"))
	_ = m.View()
	require.False(t, m.isLiveTail(live), "a user message after the live reply ends its tail role")
	require.False(t, m.materialized(live), "the old live reply is pruned once a user message follows")

	call := tools.ToolCall{ID: "ut-call", Function: tools.FunctionCall{Name: "check"}}
	_ = m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	_ = m.View()
	require.False(t, m.materialized(live), "a tool message after the old reply keeps no stale body")

	_ = m.AppendAssistantContent("s", "ut-2", "root", "next reply")
	next := len(m.views) - 1
	vprlScrollToRow(t, m, max(0, m.layoutTotal()-m.height))
	require.True(t, m.materialized(next), "precondition: the next reply is materialized at the end")
	require.True(t, m.isLiveTail(next), "the next chunk pins its own reply")
	vprlScrollToRow(t, m, 0)
	require.True(t, m.materialized(next), "the new live reply is kept at the top")
}

// TestTailRetentionCompleteSettlesOnlyItsOwnStream: a completion of another agent, or of a
// different runtime message identity, does not release this stream's pin. The matching
// completion does.
func TestTailRetentionCompleteSettlesOnlyItsOwnStream(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AppendAssistantContent("s", "w-1", "worker", tailRetentionProse)
	pinned := m.liveTailMsg
	require.NotNil(t, pinned, "the worker chunk pins its reply")

	tailRetentionComplete(m, "")
	require.Same(t, pinned, m.liveTailMsg, "another agent's completion keeps the worker pin")

	tailRetentionCompleteWithID(m, "worker", "w-0", "other message")
	require.Same(t, pinned, m.liveTailMsg, "a different message identity keeps the pin")

	tailRetentionCompleteWithID(m, "worker", "w-1", tailRetentionProse)
	require.Nil(t, m.liveTailMsg, "the matching completion releases the pin")
}

// TestTailRetentionCoalescingKeepsOnePinPerReply: chunks with one identity coalesce into one
// message that holds the pin; a new identity starts a new message and moves the pin.
func TestTailRetentionCoalescingKeepsOnePinPerReply(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AppendAssistantContent("s", "co-1", "root", "first ")
	first := m.messages[len(m.messages)-1]
	count := len(m.messages)
	_ = m.AppendAssistantContent("s", "co-1", "root", "second")
	require.Len(t, m.messages, count, "the same identity coalesces into one message")
	require.Same(t, first, m.liveTailMsg, "the coalesced chunk keeps the same pin")

	_ = m.AppendAssistantContent("s", "co-2", "root", "other reply")
	require.Len(t, m.messages, count+1, "a new identity starts a new message")
	require.Same(t, m.messages[len(m.messages)-1], m.liveTailMsg, "the pin moves to the new reply")
	require.NotSame(t, first, m.liveTailMsg)
}

// TestTailRetentionEpochDoesNotRenderOffscreenTail: a width epoch does not materialize a
// completed reply that is pruned away from the viewport.
func TestTailRetentionEpochDoesNotRenderOffscreenTail(t *testing.T) {
	m := vprlLoadRuntime(t, vprlRealSession(t, 100))
	m.suppressReplyHandoff = true
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantContent("s", "ep-1", "root", tailRetentionProse)
	tailRetentionComplete(m, tailRetentionProse)
	_ = m.View()
	tail := len(m.views) - 1
	vprlScrollToRow(t, m, 0)
	require.False(t, m.materialized(tail), "precondition: the completed reply is pruned at the top")

	_ = m.SetSize(100, 30)
	_ = m.View()
	require.False(t, m.materialized(tail), "a width epoch does not materialize the offscreen body")
}

// TestTailRetentionLiveTailReadersAreFixed pins who may touch the cache-only pin. The field
// is read only by its set and clear sites and by isLiveTail; isLiveTail is read only by
// pruneRetained. Interaction flushes never reference it, so they never release it.
func TestTailRetentionLiveTailReadersAreFixed(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, parseErr)
		files = append(files, file)
	}
	require.NotEmpty(t, files)
	fieldUsers := map[string]bool{}
	isLiveTailUsers := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "liveTailMsg":
					fieldUsers[fd.Name.Name] = true
				case "isLiveTail":
					isLiveTailUsers[fd.Name.Name] = true
				}
				return true
			})
		}
	}
	wantField := []string{"CompleteAssistant", "FinalizeStream", "isLiveTail", "liveTailSettledBy", "markLiveTail", "startSessionReplay"}
	require.Equal(t, wantField, slices.Sorted(maps.Keys(fieldUsers)), "the pin is touched only by its set, clear and read sites")
	require.Equal(t, []string{"pruneRetained"}, slices.Sorted(maps.Keys(isLiveTailUsers)), "isLiveTail retains only from pruneRetained")
}
