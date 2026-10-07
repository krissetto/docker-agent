package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestAssistantPresentationSQLiteRestart(t *testing.T) {
	for _, harnessMode := range []bool{false, true} {
		name := "model"
		if harnessMode {
			name = "harness"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ordering.db")
			store, err := sqlitestore.New(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			sess := session.New(session.WithUserMessage("synthetic task"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			observer := newPersistenceObserver(store)
			hub := newSessionEventHubWithLimits(1, 1024)
			sink := EventSinkFunc(func(event Event) {
				hub.Publish(sess.ID, event)
				observer.OnEvent(t.Context(), sess, event)
			})
			if harnessMode {
				provider := presentationHarness{events: []harness.Event{
					{Type: harness.EventReasoning, Reasoning: "first"},
					{Type: harness.EventToolCall, ToolID: "call", ToolName: "check", ToolArgs: "{}"},
					{Type: harness.EventToolResult, ToolID: "call", ToolOutput: "synthetic result"},
					{Type: harness.EventText, Text: "commentary"},
					{Type: harness.EventReasoning, Reasoning: "second"},
				}}
				a := agent.New("worker", "test", agent.WithHarness(&latest.HarnessConfig{Type: "test"}))
				rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithHarnessFactory(func(*latest.HarnessConfig) (harness.Provider, error) { return provider, nil }))
				require.NoError(t, err)
				require.Equal(t, turnEndReasonNormal, rt.runHarnessAgent(t.Context(), sess, a, sink))
			} else {
				a := agent.New("worker", "test")
				res, err := handleStream(t.Context(), nil, newStreamBuilder().AddReasoning("first").AddToolCallName("call", "check").AddToolCallArguments("call", "{}").AddContent("commentary").AddReasoning("second").Build(), a, []tools.Tool{{Name: "check"}}, sess, nil, defaultTelemetry{}, sink, time.Second)
				require.NoError(t, err)
				seed, _, cancel := hub.Subscribe(sess.ID, 16)
				cancel()
				require.Equal(t, []string{"R:first", "P:call", "C:commentary", "R:second"}, orderingSeedLabels(t, seed))
				(&LocalRuntime{now: time.Now}).recordAssistantMessage(t.Context(), sess, a, res, []tools.Tool{{Name: "check"}}, "synthetic", nil, sink)
				sink.Emit(ToolCallResponse("call", tools.Tool{Name: "check"}, tools.ResultSuccess("synthetic result"), "synthetic result", "worker"))
				result := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call", Content: "synthetic result"}}
				sess.AddMessage(result)
				sink.Emit(MessageAdded(sess.ID, result, "worker"))
			}
			want := sess.OwnMessages()[1].Message
			require.Len(t, want.Presentation, 4)
			require.Equal(t, chat.AssistantPartReasoning, want.Presentation[0].Type)
			require.Equal(t, chat.AssistantPartToolCall, want.Presentation[1].Type)
			require.Equal(t, chat.AssistantPartContent, want.Presentation[2].Type)
			require.Equal(t, chat.AssistantPartReasoning, want.Presentation[3].Type)
			require.NoError(t, observer.pendingError(sess.ID))
			require.NoError(t, store.Close())
			reopened, err := sqlitestore.New(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			got, err := reopened.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, want.Presentation, got.OwnMessages()[1].Message.Presentation)
			require.Equal(t, want.ToolCalls, got.OwnMessages()[1].Message.ToolCalls)
			if harnessMode {
				require.Empty(t, got.OwnMessages()[1].Message.ToolCalls)
				require.Equal(t, "synthetic result", *got.OwnMessages()[1].Message.Presentation[1].Tool.Result)
			} else {
				require.Equal(t, "call", got.OwnMessages()[2].Message.ToolCallID)
				require.Equal(t, "synthetic result", got.OwnMessages()[2].Message.Content)
			}
		})
	}
}
