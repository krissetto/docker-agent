package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// Tests of group commit: they drive observe(), whose batching is what they check.

func observeStream(t *testing.T, store session.Store, sess *session.Session, events []Event) []Event {
	t.Helper()
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	inner := make(chan Event, len(events))
	for _, event := range events {
		inner <- event
	}
	close(inner)
	var got []Event
	for event := range r.observe(t.Context(), sess, inner) {
		got = append(got, event)
	}
	return got
}

func TestStreamChunkBacklogIsOneCommitAndPublishesInOrder(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-backlog")
	events := []Event{
		AgentChoice("root", sess.ID, "one "),
		AgentChoiceReasoning("root", sess.ID, "think"),
		PartialToolCall(tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "read", Arguments: "{"}}, tools.Tool{Name: "read"}, "root"),
		AgentChoice("root", sess.ID, "two"),
	}
	got := observeStream(t, store, sess, events)
	assert.Equal(t, events, got, "publication keeps event order")
	// Exact structure: one batch. The gated first delta opens the placeholder, and
	// the reasoning, tool and text deltas coalesce into that same start append.
	assert.Equal(t, int64(1), store.appends.Load(), "backlog of one stream is one append")
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, "one two", rows[0].Message.Content)
	assert.Equal(t, "think", rows[0].Message.ReasoningContent)
}

func TestStreamChunkBatchBoundariesAreBarriers(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-fence")
	fence := &observerDeliveryFence{done: make(chan struct{})}
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	inner := make(chan Event, 8)
	first, second, last := AgentChoice("root", sess.ID, "a"), AgentChoice("root", sess.ID, "b"), AgentChoice("root", sess.ID, "c")
	inner <- first
	inner <- second
	inner <- fence
	inner <- last
	close(inner)
	out := r.observe(t.Context(), sess, inner)
	require.Same(t, first, <-out)
	require.Same(t, second, <-out)
	// The fence is not published; it is processed after [a b] and before [c].
	require.Same(t, last, <-out)
	select {
	case <-fence.done:
	default:
		t.Fatal("the fence must be processed before the event behind it")
	}
	_, more := <-out
	assert.False(t, more)
	assert.Equal(t, int64(2), store.appends.Load(), "[a b] then fence then [c]")
}

func TestStreamChunkByteCapSplitsBatches(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-bytes")
	big := strings.Repeat("x", observeBatchMaxBytes/2+1)
	got := observeStream(t, store, sess, []Event{AgentChoice("root", sess.ID, big), AgentChoice("root", sess.ID, big), AgentChoice("root", sess.ID, "tail")})
	assert.Len(t, got, 3)
	// [big] alone (the second big delta would exceed the cap), then [big tail].
	assert.Equal(t, int64(2), store.appends.Load())
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, big+big+"tail", rows[0].Message.Content)
}

// A batch never spans a session or agent change, even when the events are already
// queued; the first event of the other key becomes lookahead. Unscoped tool
// partials belong to the observed session and join its batch.
func TestStreamChunkDrainStopsAtBoundaries(t *testing.T) {
	otherSession := AgentChoice("root", "other", "x")
	otherAgent := AgentChoice("worker", "s", "y")
	for _, tc := range []struct {
		name string
		next Event
	}{
		{name: "session", next: otherSession},
		{name: "agent", next: otherAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := AgentChoice("root", "s", "a")
			inner := make(chan Event, 2)
			inner <- tc.next
			inner <- AgentChoice("root", "s", "b")
			batch, lookahead, hasLookahead := drainStreamChunks(inner, []Event{first}, "s")
			assert.Equal(t, []Event{first}, batch)
			require.True(t, hasLookahead)
			assert.Same(t, tc.next, lookahead)
		})
	}
	t.Run("unscoped tool partial joins", func(t *testing.T) {
		first := AgentChoice("root", "s", "a")
		partial := PartialToolCall(tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "read"}}, tools.Tool{Name: "read"}, "root")
		second := AgentChoice("root", "s", "b")
		inner := make(chan Event, 2)
		inner <- partial
		inner <- second
		batch, lookahead, hasLookahead := drainStreamChunks(inner, []Event{first}, "s")
		assert.Equal(t, []Event{first, partial, second}, batch)
		assert.Nil(t, lookahead)
		assert.False(t, hasLookahead)
	})
}

func TestStreamChunkBatchSplitsAtEventCap(t *testing.T) {
	store := newCountingStreamStore(t)
	sess := newStreamSession(t, store, "stream-batch-cap")
	inner := make(chan Event, 70)
	var want strings.Builder
	for i := range 70 {
		text := fmt.Sprintf("d%02d-", i)
		want.WriteString(text)
		inner <- AgentChoice("root", sess.ID, text)
	}
	close(inner)
	persistence := newPersistenceObserver(store)
	persistence.lifetime = t.Context()
	r := &LocalRuntime{sessionDrivers: newSessionDriverRegistry(nil), observers: []EventObserver{persistence}}
	for range r.observe(t.Context(), sess, inner) {
	}
	assert.Equal(t, int64(2), store.appends.Load(), "70 queued deltas: one 64-event batch then one 6-event batch")
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	rows := streamRows(loaded)
	require.Len(t, rows, 1)
	assert.Equal(t, want.String(), rows[0].Message.Content)
}
