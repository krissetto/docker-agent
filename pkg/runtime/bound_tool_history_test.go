package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/docker/docker-agent/pkg/hooks"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func oversizedHistory(t *testing.T) *session.Session {
	t.Helper()
	sess := session.New()
	sess.AddMessage(session.UserMessage("inspect the job"))
	sess.AddMessage(&session.Message{Message: chat.Message{
		Role:            chat.MessageRoleAssistant,
		ToolCalls:       []tools.ToolCall{{ID: "old-call", Function: tools.FunctionCall{Name: "view_background_job", Arguments: `{"job_id":"old-job"}`}}},
		ToolDefinitions: []tools.Tool{{Name: "view_background_job", Category: "background_jobs"}},
	}})
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "old-call", Content: strings.Repeat("x", 10*1024*1024) + "diagnostic tail"}})
	sess.AddMessage(&session.Message{DisplayOnly: true, Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "display-only"}})
	sess.AddMessage(session.UserMessage("continue"))
	// Exercise the persisted representation, not only an in-memory tool object.
	data, err := json.Marshal(sess)
	require.NoError(t, err)
	loaded := session.New()
	require.NoError(t, json.Unmarshal(data, loaded))
	return loaded
}

func assertBoundedHistoricalResult(t *testing.T, messages []chat.Message) {
	t.Helper()
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleTool || msg.ToolCallID != "old-call" {
			continue
		}
		assert.LessOrEqual(t, len(msg.Content), 50*1024)
		assert.Contains(t, msg.Content, "diagnostic tail")
		return
	}
	t.Fatal("historical tool result was dropped")
}

func TestRunStreamRecoversOversizedHistoricalResult(t *testing.T) {
	t.Parallel()
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "fallback"}[fallback], func(t *testing.T) {
			t.Parallel()
			prov := &recordingMsgProvider{mockProvider: mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build()}}
			a := agent.New("root", "instructions", agent.WithModel(prov))
			if fallback {
				a = agent.New("root", "instructions", agent.WithModel(&failingProvider{id: "test/primary", err: errors.New("500 simulated upstream failure")}), agent.WithFallbackModel(prov), agent.WithFallbackRetries(0))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithWorkingDir(t.TempDir()))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			sess := oversizedHistory(t)
			original := sess.Messages[2].Message.Message.Content
			for event := range rt.runExecution(t.Context(), sess) {
				if failure, ok := event.(*ErrorEvent); ok {
					t.Fatalf("unexpected runtime error: %s", failure.Error)
				}
			}
			require.NotEmpty(t, prov.got)
			for _, messages := range prov.got {
				assertBoundedHistoricalResult(t, messages)
			}
			assert.Equal(t, original, sess.Messages[2].Message.Message.Content)
			assert.Equal(t, "recovered", sess.GetLastAssistantMessageContent())
		})
	}
}

func TestHistoricalToolBoundsFollowTransformsAndPrecedePolicies(t *testing.T) {
	t.Parallel()
	for _, transforms := range []bool{false, true} {
		t.Run(fmt.Sprintf("transforms=%t", transforms), func(t *testing.T) {
			t.Parallel()
			a := agent.New("root", "instructions")
			sess := oversizedHistory(t)
			msgs, _, _ := sess.CompactionInput()
			original := msgs[2].Content
			r := &LocalRuntime{}
			if transforms {
				r.transforms = append(r.transforms, registeredTransform{name: "expand", fn: func(_ context.Context, _ *hooks.Input, messages []chat.Message) ([]chat.Message, error) {
					messages[2].Content = original + "transformed tail"
					return messages, nil
				}})
			}
			projected := r.applyBeforeLLMCallTransforms(t.Context(), sess, a, "test/model", nil, msgs)
			assertBoundedHistoricalResult(t, projected)
			if transforms {
				assert.Contains(t, projected[2].Content, "transformed tail")
			}
			r.transforms = append(r.transforms, registeredTransform{name: "policy", policy: true, fn: func(_ context.Context, _ *hooks.Input, messages []chat.Message) ([]chat.Message, error) {
				assertBoundedHistoricalResult(t, messages)
				for _, msg := range messages {
					assert.NotEqual(t, "display-only", msg.Content)
				}
				return nil, errors.New("deny delivery")
			}})
			_, err := r.applyMessagePolicies(t.Context(), sess, a, "test/model", nil, projected)
			require.ErrorContains(t, err, "deny delivery")
			assert.Equal(t, original, msgs[2].Content)
			assert.Equal(t, original, sess.Messages[2].Message.Message.Content)
		})
	}
}
