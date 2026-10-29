package messages

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Gated: DA_RETAINED_MEMORY_REPORT=1 go test -run TestRetainedMemoryReport -v ./pkg/tui/components/messages/
// Optional: DA_RETAINED_MEMORY_MODE=compact|plain (default compact: Compact Work
// on, as in the app default; plain: Compact Work off). Run each mode in its own
// process so post-GC deltas stay isolated.
// DA_RETAINED_MEMORY_MESSAGES=N (generator records, default 20000; the workload
// emits 1.2N session items because case-6/9 records add a tool-call item),
// DA_RETAINED_MEMORY_PROFILE=<path> writes a post-GC heap profile.
func TestRetainedMemoryReport(t *testing.T) {
	if os.Getenv("DA_RETAINED_MEMORY_REPORT") == "" {
		t.Skip("set DA_RETAINED_MEMORY_REPORT=1 to run the retained-memory report")
	}
	records := 20000
	if v, err := strconv.Atoi(os.Getenv("DA_RETAINED_MEMORY_MESSAGES")); err == nil && v > 0 {
		records = v
	}

	mode := os.Getenv("DA_RETAINED_MEMORY_MODE")
	if mode == "" {
		mode = "compact"
	}
	if mode != "compact" && mode != "plain" {
		t.Fatalf("DA_RETAINED_MEMORY_MODE must be compact or plain, got %q", mode)
	}

	sess := retainedMemoryWorkload(records)
	before := retainedHeapAfterGC()

	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	if mode == "compact" {
		_ = m.SetCompactWorkLog(true)
	}
	BeginReplay(m, PrepareReplay(sess), nil)
	drainRetainedReplay(t, m)
	finishHistoryReplay(t, m)

	after := retainedHeapAfterGC()
	counts := map[types.MessageType]int{}
	var liveRenderViews, lineBytes int
	for i, msg := range m.messages {
		counts[msg.Type]++
		if mv, ok := m.views[i].(interface{ HasLiveRenderState() bool }); ok && mv.HasLiveRenderState() {
			liveRenderViews++
		}
	}
	for _, line := range m.renderedLines {
		lineBytes += len(line)
	}
	rssKB := residentKB(t.Context())

	t.Logf("mode=%s", mode)
	t.Logf("workload: generator_records=%d session_items=%d visible_views=%d transcript_lines=%d transcript_line_bytes=%d history_pending=%v",
		records, len(sess.Messages), len(m.views), len(m.renderedLines), lineBytes, HistoryPending(m))
	for _, typ := range []types.MessageType{types.MessageTypeUser, types.MessageTypeAssistant, types.MessageTypeAssistantReasoningBlock, types.MessageTypeToolCall, types.MessageTypeToolResult} {
		t.Logf("views_by_type: %v=%d", typ, counts[typ])
	}
	t.Logf("live_render_state_views=%d", liveRenderViews)
	t.Logf("heap_retained_bytes=%d (%.2f MiB) heap_before=%d heap_after=%d", int64(after.HeapAlloc)-int64(before.HeapAlloc), mib(int64(after.HeapAlloc)-int64(before.HeapAlloc)), before.HeapAlloc, after.HeapAlloc)
	t.Logf("heap_inuse_after=%.2f MiB rss_kb=%d", mib(int64(after.HeapInuse)), rssKB)
	t.Logf("retained_bytes_per_session_item=%.1f", float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/float64(len(sess.Messages)))

	if path := os.Getenv("DA_RETAINED_MEMORY_PROFILE"); path != "" {
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, pprof.WriteHeapProfile(f))
		require.NoError(t, f.Close())
		t.Logf("heap profile written to %s", path)
	}
	runtime.KeepAlive(sess)
	runtime.KeepAlive(m)
}

func retainedHeapAfterGC() runtime.MemStats {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats
}

func residentKB(ctx context.Context) int64 {
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

func mib(bytes int64) float64 { return float64(bytes) / (1 << 20) }

// retainedMemoryWorkload is a deterministic mix: per 10 records, 1 user prompt,
// 4 markdown replies with code, 1 reasoning+reply, 2 shell tool calls with
// results, 1 larger markdown reply and 1 short reply. Records 6 and 9 emit an
// extra assistant tool-call item before their tool result item, so n records
// yield 1.2n session items.
func retainedMemoryWorkload(n int) *session.Session {
	sess := session.New(session.WithID("retained-memory"))
	for i := range n {
		id := strconv.Itoa(i)
		var item session.Item
		switch i % 10 {
		case 0:
			item = session.NewMessageItem(&session.Message{Message: chat.Message{
				Role:    chat.MessageRoleUser,
				Content: "Please review change " + id + " in the billing module and summarize the risks for the release.",
			}})
		case 1, 2, 3, 4, 8:
			body := fmt.Sprintf("## Finding %s\n\nThe worker checked **module %s** and found `value_%s` in the cache layer. "+
				"Retention depends on *width* and on [the report](https://example.com/%s).\n\n"+
				"- first observation for item %s with `inline code`\n- second observation for item %s\n\n"+
				"```go\nfunc handle%s(input string) error {\n\treturn fmt.Errorf(\"item %s: %%s\", input)\n}\n```\n\n"+
				"Closing remark %s with a few more words so the paragraph wraps across the transcript width.\n",
				id, id, id, id, id, id, id, id, id)
			if i%10 == 8 {
				body += strings.Repeat("Additional paragraph describing the change in detail. ", 12) + "\n"
			}
			item = session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: body}})
		case 5:
			item = session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{
				Role:             chat.MessageRoleAssistant,
				ReasoningContent: "Checking the billing ledger invariants for item " + id + " before answering. " + strings.Repeat("Considering edge cases. ", 6),
				Content:          "Reasoned answer for item " + id + ": no blocking issue.",
			}})
		case 6, 9:
			call := tools.ToolCall{ID: "call-" + id, Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"go test ./pkg/billing/..."}`}}
			sess.Messages = append(sess.Messages, session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}}))
			item = session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{
				Role: chat.MessageRoleTool, ToolCallID: call.ID,
				Content: "ok  \tbilling/ledger\t0.42s\nok  \tbilling/invoice\t0.13s\n",
			}})
		case 7:
			item = session.NewMessageItem(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "Done with item " + id + "."}})
		}
		sess.Messages = append(sess.Messages, item)
	}
	return sess
}

// drainRetainedReplay runs a 20k-item replay to completion; finishPreparedReplay caps at 100 slices.
func drainRetainedReplay(tb testing.TB, m *model) {
	tb.Helper()
	for range 1_000_000 {
		done, cmd := ContinueReplay(m)
		runReplayCommand(m, cmd)
		if done {
			return
		}
	}
	tb.Fatal("replay did not finish")
}
