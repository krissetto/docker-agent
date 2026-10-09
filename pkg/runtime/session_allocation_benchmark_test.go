package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// A long tool-using transcript, including the nested schema and presentation
// state copied by real history snapshots (not just repeated plain text).
func allocationHistoryFixture(id, parentID string, turns int) *session.Session {
	createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sess := session.New(session.WithID(id), session.WithParentID(parentID), session.WithAgentName("root"), session.WithClock(func() time.Time { return createdAt }))
	for i := range 40 {
		sess.SetAttribute(fmt.Sprintf("embedder.setting.%d", i), strings.Repeat("setting ", 8))
	}
	result := strings.Repeat("pkg/runtime/session_driver.go: canonical owner checked\n", 80)
	for i := range turns {
		call := tools.ToolCall{ID: fmt.Sprintf("call-%d", i), Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rg -n sessionDriver pkg/runtime","timeout":30}`}}
		definition := tools.Tool{Name: "shell", Description: "Run a shell command in the workspace", Parameters: map[string]any{
			"type": "object", "required": []any{"cmd"},
			"properties": map[string]any{
				"cmd":     map[string]any{"type": "string", "description": "Shell command"},
				"timeout": map[string]any{"type": "integer", "minimum": float64(0)},
				"options": map[string]any{"type": "object", "properties": map[string]any{
					"env": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				}},
			},
		}, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"output": map[string]any{"type": "string"}}}, Metadata: map[string]string{"source": "builtin"}}
		sess.AddMessage(session.UserMessageAt(createdAt, fmt.Sprintf("Inspect and validate allocation change %d", i)))
		sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{
			Role: chat.MessageRoleAssistant, Content: "Inspecting the relevant code before changing it.",
			ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{definition},
			Presentation: []chat.AssistantPart{
				{Type: chat.AssistantPartReasoning, Text: "Check canonical ownership and avoid unnecessary snapshots."},
				{Type: chat.AssistantPartToolCall, ToolCallID: call.ID, Tool: &chat.AssistantTool{Call: call, Definition: definition, Result: &result}},
			},
		}})
		sess.AddMessage(&session.Message{AgentName: "root", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result}})
	}
	return sess
}

func BenchmarkSessionAllocationHotPaths(b *testing.B) {
	root := allocationHistoryFixture("bench-root", "", 256)
	child := allocationHistoryFixture("bench-child", root.ID, 32)
	root.AddLiveSubSession(child)
	root.SetAttribute(SessionDelegationAttribute, "true")
	encoded, err := json.Marshal(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("fixture messages=864 JSON-bytes=%d SHA256=%x", len(encoded), sha256.Sum256(encoded))
	fixtureTime := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r := &LocalRuntime{now: func() time.Time { return fixtureTime }}
	r.sessionDrivers = newSessionDriverRegistry(r)
	r.sessionDrivers.drivers[root.ID] = &sessionDriver{sess: root}
	r.sessionDrivers.drivers[child.ID] = &sessionDriver{sess: child}
	handle := &sessionHandle{runtime: r, driver: r.sessionDrivers.drivers[child.ID]}
	b.Run("DelegationRoot", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if !r.sessionDelegationEnabled(root) {
				b.Fatal("delegation unexpectedly disabled")
			}
		}
	})
	b.Run("DelegationDescendantHandle", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			enabled, err := handle.DelegationPolicy(context.Background())
			if err != nil || !enabled {
				b.Fatalf("delegation query: %v, %v", enabled, err)
			}
		}
	})
	b.Run("LastAssistantContent", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if root.GetLastAssistantMessageContent() == "" {
				b.Fatal("missing assistant content")
			}
		}
	})
	b.Run("LastUserContent", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if root.GetLastUserMessageContent() == "" {
				b.Fatal("missing user content")
			}
		}
	})
	for _, setting := range []struct {
		name  string
		level slog.Level
	}{{"DebugDisabled", slog.LevelInfo}, {"DebugEnabled", slog.LevelDebug}} {
		b.Run("RecordAssistant"+setting.name, func(b *testing.B) {
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: setting.level})))
			defer slog.SetDefault(previous)
			a := agent.New("root", "prompt")
			sink := EventSinkFunc(func(Event) {})
			res := streamResult{Content: "The canonical owner and snapshot isolation checks passed."}
			n := len(root.Messages)
			b.ReportAllocs()
			for b.Loop() {
				r.recordAssistantMessage(context.Background(), root, a, res, nil, "test/fixture", nil, sink)
				root.Messages = root.Messages[:n]
			}
		})
	}
}
