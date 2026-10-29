package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// ToolDefinitions and Presentation share schemas so cloning exposes duplicate copies.
func investigationFixture(n, props int) *Session {
	createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sess := New(WithID("investigation"), WithAgentName("root"), WithClock(func() time.Time { return createdAt }))
	result := strings.Repeat("pkg/session/session.go: snapshot checked\n", 6)
	for i := 0; sess.AllMessageCount() < n; i++ {
		call := tools.ToolCall{ID: fmt.Sprintf("call-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rg -n snapshot pkg/session"}`}}
		var definition tools.Tool
		var defs []tools.Tool
		if props >= 0 {
			definition = tools.Tool{Name: "shell", Description: "Run a shell command", Parameters: investigationToolSchema(props)}
			defs = []tools.Tool{definition}
		}
		msgs := []*Message{
			UserMessageAt(createdAt, fmt.Sprintf("Inspect snapshot ownership step %d", i)),
			{AgentName: "root", Message: chat.Message{
				Role: chat.MessageRoleAssistant, Content: "Inspecting the code before changing it.",
				ToolCalls: []tools.ToolCall{call}, ToolDefinitions: defs,
				Presentation: []chat.AssistantPart{{
					Type: chat.AssistantPartToolCall, ToolCallID: call.ID,
					Tool: &chat.AssistantTool{Call: call, Definition: definition, Result: &result},
				}},
			}},
			{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result}},
		}
		for _, msg := range msgs {
			if sess.AllMessageCount() >= n {
				break
			}
			sess.AddMessage(msg)
		}
	}
	return sess
}

func investigationToolSchema(props int) map[string]any {
	properties := make(map[string]any, props)
	for i := range props {
		properties[fmt.Sprintf("arg%d", i)] = map[string]any{"type": "string", "description": "argument description"}
	}
	return map[string]any{"type": "object", "required": []any{"arg0"}, "properties": properties}
}

var investigationSizes = []int{1000, 5000, 20000}

func BenchmarkInvestigationHistoryReads(b *testing.B) {
	for _, n := range investigationSizes {
		sess := investigationFixture(n, -1)
		require.Equal(b, n, sess.AllMessageCount())
		b.Run(fmt.Sprintf("messages=%d/ItemsSnapshot", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = sess.ItemsSnapshot()
			}
		})
		b.Run(fmt.Sprintf("messages=%d/GetAllMessages", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = sess.GetAllMessages()
			}
		})
		b.Run(fmt.Sprintf("messages=%d/LastAssistantContent", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = sess.GetLastAssistantMessageContent()
			}
		})
		b.Run(fmt.Sprintf("messages=%d/MarshalJSON", n), func(b *testing.B) {
			b.ReportAllocs()
			var size int
			for b.Loop() {
				data, err := json.Marshal(sess)
				require.NoError(b, err)
				size = len(data)
			}
			b.ReportMetric(float64(size), "json-bytes")
		})
		runtime.KeepAlive(sess)
	}
}

func BenchmarkInvestigationSnapshotSchema(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		for _, props := range []int{0, 16, 64} {
			sess := investigationFixture(n, props)
			b.Run(fmt.Sprintf("messages=%d/props=%d/Clone", n, props), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = sess.Clone()
				}
			})
			b.Run(fmt.Sprintf("messages=%d/props=%d/OwnSnapshot", n, props), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = sess.OwnSnapshot()
				}
			})
			b.Run(fmt.Sprintf("messages=%d/props=%d/ItemsSnapshot", n, props), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					_ = sess.ItemsSnapshot()
				}
			})
			runtime.KeepAlive(sess)
		}
	}
}

// Serialize after heap samples to exclude JSON scratch from retained graphs.
func TestInvestigationHistoryRetention(t *testing.T) {
	if os.Getenv("INVESTIGATION_RETENTION") != "1" {
		t.Skip("set INVESTIGATION_RETENTION=1")
	}
	if os.Getenv("INVESTIGATION_PROFILE_DIR") != "" {
		runtime.MemProfileRate = 4096
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	for _, n := range []int{1000, 5000} {
		for _, props := range []int{-1, 16, 64} {
			base := heap()
			sess := investigationFixture(n, props)
			withSource := heap()
			snap := sess.Clone()
			withBoth := heap()
			writeInvestigationLiveProfile(t, n, props, "source-and-clone")
			runtime.KeepAlive(sess)
			sess = nil
			snapOnly := heap()
			runtime.KeepAlive(sess)
			writeInvestigationLiveProfile(t, n, props, "clone-only")
			data, err := json.Marshal(snap)
			require.NoError(t, err)
			jsonBytes := len(data)
			runtime.KeepAlive(snap)
			snap = nil
			afterDrop := heap()
			runtime.KeepAlive(snap)
			t.Logf("messages=%d props=%d source_bytes=%d clone_bytes=%d snapshot_only_bytes=%d per_message=%.1f json_bytes=%d residual_after_drop=%d",
				n, props, int64(withSource)-int64(base), int64(withBoth)-int64(withSource), int64(snapOnly)-int64(base),
				float64(int64(snapOnly)-int64(base))/float64(n), jsonBytes, int64(afterDrop)-int64(base))
		}
	}
}

// Capture reachable graphs, not an exit profile dominated by package initialization.
func writeInvestigationLiveProfile(t *testing.T, n, props int, phase string) {
	t.Helper()
	dir := os.Getenv("INVESTIGATION_PROFILE_DIR")
	if dir == "" || n != 5000 || props != 64 {
		return
	}
	f, err := os.Create(filepath.Join(dir, "retention-live-5k-p64-"+phase+".heap.prof"))
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, pprof.Lookup("heap").WriteTo(f, 0))
}
