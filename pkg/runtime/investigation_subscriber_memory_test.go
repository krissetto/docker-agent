package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func invMemHub() *sessionEventHub {
	return newSessionEventHubWithLimits(1024, 8<<20)
}

func invMemChunk(size int) Event {
	return AgentChoice("root", "s", strings.Repeat("x", size))
}

func invMemHeavy(tb testing.TB) {
	tb.Helper()
	if os.Getenv("INVMEM_HEAVY") != "1" {
		tb.Skip("set INVMEM_HEAVY=1 for the quadratic in-flight text fixture")
	}
}

// Diagnostic characterization of current retention; opt in with INVMEM_DIAGNOSTICS=1.
func invMemRequireDiagnostics(tb testing.TB) {
	tb.Helper()
	if os.Getenv("INVMEM_DIAGNOSTICS") != "1" {
		tb.Skip("set INVMEM_DIAGNOSTICS=1 to run diagnostic characterization")
	}
}

func invMemInflightText(h *sessionEventHub, sessionID string) int {
	st := h.inflight[sessionID]
	if st == nil {
		return 0
	}
	text := 0
	for _, part := range st.parts {
		text += len(part.Text)
	}
	return text
}

func invMemDrainNow(ch <-chan Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestInvMemSlowObserverIsDisconnectedNotBuffered(t *testing.T) {
	h := invMemHub()
	_, ch, cancel := h.Subscribe("s", 4)
	defer cancel()
	for range 20 {
		h.Publish("s", invMemChunk(256))
	}
	received := 0
	for range ch {
		received++
	}
	assert.LessOrEqual(t, received, 5, "buffer+1 bounds retained backlog for a stalled observer")
	assert.Zero(t, h.subscriberCount)
	assert.Empty(t, h.subs["s"])
}

func TestInvMemSlowSequencedObserverGetsGapAndReleases(t *testing.T) {
	h := invMemHub()
	_, ch, cancel, _ := h.SubscribeSequenced("s", nil, 4)
	defer cancel()
	for range 20 {
		h.Publish("s", invMemChunk(256))
	}
	var last SequencedSessionEvent
	received := 0
	for ev := range ch {
		last = ev
		received++
	}
	assert.True(t, last.Gap, "overflowed observer must see a gap, not a clean close")
	assert.LessOrEqual(t, received, 5)
	assert.Zero(t, h.subscriberCount)
	assert.Empty(t, h.seqSubs["s"])
}

func TestInvMemSubscribeCancelCyclesReleaseHubState(t *testing.T) {
	h := invMemHub()
	for range 2000 {
		_, ch, cancel := h.Subscribe("s", 8)
		h.Publish("s", invMemChunk(256))
		cancel()
		for range ch {
		}
	}
	assert.Zero(t, h.subscriberCount)
	assert.Empty(t, h.subs)
}

func TestInvMemDeletedSessionTombstonesAccumulate(t *testing.T) {
	invMemRequireDiagnostics(t)
	h := invMemHub()
	for i := range 1000 {
		id := fmt.Sprintf("s-%d", i)
		h.Publish(id, StreamStarted(id, "root"))
		h.Delete(id)
	}
	t.Logf("closed tombstones=%d replay sessions=%d nextSeq sessions=%d", len(h.closed), len(h.replay), len(h.nextSeq))
	assert.Len(t, h.closed, 1000, "documents per-deleted-session tombstones kept for the process lifetime")
	assert.Empty(t, h.replay)
	assert.Empty(t, h.nextSeq)
}

// Small chunks keep the per-publish text concatenation cheap while still evicting replay.
func TestInvMemReplayRetentionStaysWithinCapacity(t *testing.T) {
	h := invMemHub()
	const chunks, size = 2000, 16
	for range chunks {
		h.Publish("s", invMemChunk(size))
	}
	assert.Len(t, h.replay["s"], 1024)
	assert.LessOrEqual(t, h.bytes["s"], 8<<20)
	text := invMemInflightText(h, "s")
	t.Logf("replay=%d replayBytes=%d inflightTextBytes=%d", len(h.replay["s"]), h.bytes["s"], text)
	assert.Equal(t, chunks*size, text, "in-flight text must equal the published chunks")
}

func TestInvMemInflightSeedAdmissionRejectsOversizedState(t *testing.T) {
	h := invMemHub()
	h.inflight["s"] = &inflightAssistant{parts: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: strings.Repeat("x", 8<<20+1)}}}
	seed, ch, cancel := h.Subscribe("s", 8)
	defer cancel()
	assert.Nil(t, seed)
	_, open := <-ch
	assert.False(t, open, "oversized live seed is rejected with a closed channel")
}

// Heavy: 136 x 64KiB publishes exceed the 8MiB seed limit; quadratic copy is ~0.6GB.
func TestInvMemInflightSeedAdmissionAfterPublishHeavy(t *testing.T) {
	invMemHeavy(t)
	invMemRequireDiagnostics(t)
	h := invMemHub()
	const chunks, size = 136, 64 << 10
	for range chunks {
		h.Publish("s", invMemChunk(size))
	}
	text := invMemInflightText(h, "s")
	require.Greater(t, text, 8<<20, "fixture must exceed the seed admission limit")
	seed, ch, cancel := h.Subscribe("s", 8)
	defer cancel()
	assert.Nil(t, seed)
	_, open := <-ch
	assert.False(t, open, "publish accepted state that seed admission then rejects")
	t.Logf("published %d chunks x %dB; inflight text=%dB", chunks, size, text)
}

func BenchmarkInvMemHubPublish(b *testing.B) {
	const batch = 512
	for _, observers := range []int{0, 1, 8, 32} {
		b.Run(fmt.Sprintf("observers=%d", observers), func(b *testing.B) {
			h := invMemHub()
			chans := make([]<-chan Event, 0, observers)
			cancels := make([]func(), 0, observers)
			for range observers {
				_, ch, cancel := h.Subscribe("s", 1024)
				chans = append(chans, ch)
				cancels = append(cancels, cancel)
			}
			ev := Warning("invmem", "root")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 1; i <= b.N; i++ {
				h.Publish("s", ev)
				if i%batch == 0 || i == b.N {
					b.StopTimer()
					for _, ch := range chans {
						invMemDrainNow(ch)
					}
					b.StartTimer()
				}
			}
			b.StopTimer()
			h.mu.Lock()
			got := h.subscriberCount
			h.mu.Unlock()
			if got != observers {
				b.Fatalf("observers disconnected: have %d want %d", got, observers)
			}
			for _, cancel := range cancels {
				cancel()
			}
		})
	}
}

// Quadratic in b.N: each chunk re-concatenates the in-flight text; run with small -benchtime.
func BenchmarkInvMemInflightChunks(b *testing.B) {
	h := invMemHub()
	ev := invMemChunk(256)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		h.Publish("s", ev)
	}
}

func invMemHeap() goruntime.MemStats {
	goruntime.GC()
	goruntime.GC()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return m
}

func invMemWriteHeapProfile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pprof.WriteHeapProfile(f))
}

// Gated: live heap after GC while the hub owns replay and in-flight text, then after Delete.
func TestInvMemHeapRetainedByReplayAndInflight(t *testing.T) {
	if os.Getenv("INVMEM_HEAP") != "1" {
		t.Skip("set INVMEM_HEAP=1 to measure post-GC heap retained by hub replay and in-flight text")
	}
	profile := os.Getenv("INVMEM_HEAP_PROFILE")
	const events, size = 1000, 512
	before := invMemHeap()
	h := invMemHub()
	for range events {
		h.Publish("s", invMemChunk(size))
	}
	live := invMemHeap()
	require.Len(t, h.replay["s"], events)
	text := invMemInflightText(h, "s")
	if profile != "" {
		invMemWriteHeapProfile(t, profile+".live.pb.gz")
	}
	goruntime.KeepAlive(h)
	h.Delete("s")
	after := invMemHeap()
	if profile != "" {
		invMemWriteHeapProfile(t, profile+".afterdelete.pb.gz")
	}
	goruntime.KeepAlive(h)
	t.Logf("events=%d payloadBytes=%d inflightText=%d heapAlloc before=%d live=%d afterDelete=%d deltaLive=%d deltaAfterDelete=%d",
		events, size, text, before.HeapAlloc, live.HeapAlloc, after.HeapAlloc,
		int64(live.HeapAlloc)-int64(before.HeapAlloc), int64(after.HeapAlloc)-int64(before.HeapAlloc))
}

func TestInvMemObserversReceiveIndependentCopies(t *testing.T) {
	h := invMemHub()
	_, first, cancelFirst := h.Subscribe("s", 8)
	defer cancelFirst()
	_, second, cancelSecond := h.Subscribe("s", 8)
	defer cancelSecond()
	h.Publish("s", AgentChoice("root", "s", "original"))
	got := (<-first).(*AgentChoiceEvent)
	got.Content = "mutated"
	require.Equal(t, "original", (<-second).(*AgentChoiceEvent).Content)
}

func TestInvMemWireDecodeMatchesSnapshotForProcessLocalMetadata(t *testing.T) {
	msg := &session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "hi"}}
	added := MessageAdded("s", msg, "root").(*MessageAddedEvent)
	added.boundaryOnly = true
	summary := SessionSummary("s", "sum", "root", 2, 0.5, "m", nil).(*SessionSummaryEvent)
	summary.persisted = true
	call := ToolCall(tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}, tools.Tool{Name: "read"}, "root")
	for _, ev := range []Event{added, summary, call} {
		want, err := observerEventSnapshot(ev)
		require.NoError(t, err)
		wire, err := json.Marshal(ev)
		require.NoError(t, err)
		got, err := observerEventFromWire(ev, wire)
		require.NoError(t, err)
		require.Equal(t, want, got, "%T", ev)
	}
	detached, err := observerEventSnapshot(added)
	require.NoError(t, err)
	require.True(t, detached.(*MessageAddedEvent).boundaryOnly, "boundary flag must survive detaching")
	require.NotSame(t, msg, detached.(*MessageAddedEvent).Message, "detached message must be an independent copy")
	detachedSummary, err := observerEventSnapshot(summary)
	require.NoError(t, err)
	require.True(t, detachedSummary.(*SessionSummaryEvent).persisted, "persisted flag must survive detaching")
}
