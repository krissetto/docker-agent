package embeddedchat

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type blockingToolSet struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	stopped   atomic.Int32
}

func (b *blockingToolSet) Start(context.Context) error { return nil }
func (b *blockingToolSet) Stop(context.Context) error  { b.stopped.Add(1); return nil }
func (b *blockingToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "block", Parameters: map[string]any{"type": "object"}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		close(b.started)
		<-ctx.Done()
		close(b.cancelled)
		<-b.release
		return nil, ctx.Err()
	}}}, nil
}

type blockingToolProvider struct {
	stubProvider

	calls atomic.Int32
}

func (p *blockingToolProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	delta := chat.MessageDelta{Content: "next turn"}
	if p.calls.Add(1) == 1 {
		delta = chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "block-1", Type: "function", Function: tools.FunctionCall{Name: "block", Arguments: "{}"}}}}
	}
	return &sequentialReplyStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: delta}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}},
	}}, nil
}

func TestSessionDrainsRealToolBeforeReleasingOwnership(t *testing.T) {
	for _, operation := range []string{"cancel", "close"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			tool := &blockingToolSet{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(tool.release) })
			provider := &blockingToolProvider{}
			tm := team.New(team.WithAgents(agent.New("root", "Use the tool.", agent.WithModel(provider), agent.WithToolSets(tool))))
			s, err := New(t.Context(), Config{Team: tm, SessionOptions: []session.Opt{session.WithToolsApproved(true)}})
			require.NoError(t, err)
			t.Cleanup(func() {
				release()
				require.NoError(t, s.Close())
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
				defer cancel()
				require.NoError(t, tm.StopToolSets(ctx))
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			out, err := s.Send(ctx, "first")
			require.NoError(t, err)
			select {
			case <-tool.started:
			case <-time.After(5 * time.Second):
				t.Fatal("tool did not start")
			}
			closeDone := make(chan error, 1)
			if operation == "close" {
				go func() { closeDone <- s.Close() }()
			} else {
				cancel()
			}
			select {
			case <-tool.cancelled:
			case <-time.After(time.Second):
				t.Fatal("caller cancellation detached observation without cancelling tool")
			}
			next, err := s.Send(t.Context(), "too soon")
			if operation == "close" {
				require.ErrorIs(t, err, ErrClosed)
				select {
				case <-closeDone:
					t.Fatal("Close returned while its tool was still running")
				default:
				}
			} else {
				require.ErrorIs(t, err, ErrRunActive)
			}
			require.Nil(t, next)
			assert.Equal(t, int32(1), provider.calls.Load())
			release()
			for event := range out {
				assert.False(t, event.Done, "cancelled sends must not report success")
			}
			if operation == "close" {
				select {
				case err := <-closeDone:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("Close did not drain after tool cleanup")
				}
			} else {
				next, err = s.Send(t.Context(), "second")
				require.NoError(t, err)
				var done bool
				for event := range next {
					require.NoError(t, event.Err)
					done = done || event.Done
				}
				require.True(t, done)
				snapshot, err := s.Conversation(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "next turn", snapshot.GetLastAssistantMessageContent())
				require.NoError(t, s.Close())
			}
			assert.Zero(t, tool.stopped.Load(), "prebuilt team is borrowed, not owned by embedded session")
		})
	}
}
