package root

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/shellprocess"
)

type execWorkflowProvider struct {
	rootTestProvider
	call func(context.Context) (chat.MessageStream, error)
}

func (p execWorkflowProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return p.call(ctx)
}

type execWorkflowModelStore struct{ runtime.ModelStore }

func (execWorkflowModelStore) GetModel(context.Context, modelsdev.ID) (*modelsdev.Model, error) {
	return nil, nil
}

type execWorkflowStream struct {
	chunks []chat.MessageStreamResponse
	index  int
}

func (s *execWorkflowStream) Recv() (chat.MessageStreamResponse, error) {
	if s.index == len(s.chunks) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}
func (*execWorkflowStream) Close() {}
func execWorkflowResponse(delta chat.MessageDelta, finish chat.FinishReason) chat.MessageStreamResponse {
	return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: delta, FinishReason: finish}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}
}

func TestExecWorkflowWaitsForAsyncChildAndRootFollowUp(t *testing.T) {
	for _, outcome := range []string{"success", "cancel", "root error", "child error"} {
		for _, jsonMode := range []bool{false, true} {
			name := "text"
			if jsonMode {
				name = "json"
			}
			t.Run(outcome+"/"+name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				defer releaseOnce.Do(func() { close(release) })
				var enteredOnce, canceledOnce sync.Once
				var calls atomic.Int32
				rootModel := execWorkflowProvider{call: func(ctx context.Context) (chat.MessageStream, error) {
					if calls.Add(1) == 1 {
						return &execWorkflowStream{chunks: []chat.MessageStreamResponse{
							execWorkflowResponse(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "spawn", Type: "function", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker","task":"remain busy"}`}}}}, chat.FinishReasonToolCalls),
						}}, nil
					}
					select {
					case <-entered:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					content := "Root initial turn complete."
					if calls.Load() > 2 {
						if outcome == "root error" {
							return nil, errors.New("root follow-up failed")
						}
						content = "Root report follow-up complete."
					}
					return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: content}, chat.FinishReasonStop)}}, nil
				}}
				workerModel := execWorkflowProvider{call: func(ctx context.Context) (chat.MessageStream, error) {
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
						if outcome == "child error" {
							return nil, errors.New("child failed")
						}
						return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: "Worker complete."}, chat.FinishReasonStop)}}, nil
					case <-ctx.Done():
						canceledOnce.Do(func() { close(canceled) })
						return nil, ctx.Err()
					}
				}}
				root := agent.New("root", "spawn worker then finish", agent.WithModel(rootModel), agent.WithTitleModel(titleTestProvider{calls: &atomic.Int32{}}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}), agent.WithToolSets(subagent.NewToolSet()))
				worker := agent.New("worker", "remain busy", agent.WithModel(workerModel), agent.WithTitleModel(titleTestProvider{calls: &atomic.Int32{}}))
				rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root, worker)), runtime.WithSessionStore(session.NewInMemorySessionStore()), runtime.WithModelStore(execWorkflowModelStore{}), runtime.WithSessionCompaction(false))
				require.NoError(t, err)
				owner := runtime.NewSessionRuntimeSupervisor(rt)
				defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(ctx))) }()
				sess := session.New(session.WithID("exec-workflow-"+name), session.WithAgentName("root"), session.WithTitle("ExecWorkflow"), session.WithToolsApproved(true))
				var output bytes.Buffer
				flags := &runExecFlags{exec: true, outputJSON: jsonMode}
				done := make(chan error, 1)
				go func() {
					done <- flags.handleExecMode(ctx, cli.NewPrinter(&output), rt, owner.Runtime(), sess, []string{"mock-team", "spawn and finish"})
				}()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("child did not start")
				}
				select {
				case err := <-done:
					t.Fatalf("exec exited with child running: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				if outcome == "cancel" {
					cancel()
				} else {
					releaseOnce.Do(func() { close(release) })
				}
				select {
				case err := <-done:
					if outcome == "cancel" {
						require.ErrorIs(t, err, context.Canceled)
						require.False(t, rt.HasRunningSubagents(sess.ID))
						select {
						case <-canceled:
						default:
							t.Fatal("workflow cancellation did not cancel child")
						}
						return
					}
					if outcome == "child error" {
						require.ErrorContains(t, err, "child failed")
						require.Contains(t, output.String(), "Root report follow-up complete.")
						return
					}
					if outcome == "root error" {
						require.ErrorContains(t, err, "root follow-up failed")
						return
					}
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("workflow did not finish")
				}
				require.False(t, rt.HasRunningSubagents(sess.ID))
				require.Contains(t, output.String(), "Root initial turn complete.")
				require.Contains(t, output.String(), "Root report follow-up complete.")
				select {
				case <-canceled:
					t.Fatal("successful workflow canceled child")
				default:
				}

			})
		}
	}
}

func TestExecWorkflowWithoutSubagents(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		name := "text"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			model := execWorkflowProvider{call: func(context.Context) (chat.MessageStream, error) {
				return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: "done"}, chat.FinishReasonStop)}}, nil
			}}
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(model)))), runtime.WithSessionStore(session.NewInMemorySessionStore()), runtime.WithModelStore(execWorkflowModelStore{}), runtime.WithSessionCompaction(false))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
			var output bytes.Buffer
			flags := &runExecFlags{exec: true, outputJSON: jsonMode}
			require.NoError(t, flags.handleExecMode(t.Context(), cli.NewPrinter(&output), rt, owner.Runtime(), session.New(session.WithAgentName("root")), []string{"mock-team", "first", "second"}))
			require.Contains(t, output.String(), "done")
		})
	}
}

type execBackgroundProvider struct {
	rootTestProvider
	call func(context.Context, []chat.Message) (chat.MessageStream, error)
}

func (p execBackgroundProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return p.call(ctx, messages)
}

func TestExecWorkflowWaitsForAutomaticShellJobAndCompletionFollowup(t *testing.T) {
	started := time.Now()
	var calls atomic.Int32
	completionSeen := make(chan bool, 1)
	model := execBackgroundProvider{call: func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
		switch calls.Add(1) {
		case 1:
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "shell", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"sleep 1.1; echo same-process-complete","timeout":5}`}}}}, chat.FinishReasonToolCalls)}}, nil
		case 2:
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: "initial response"}, chat.FinishReasonStop)}}, nil
		default:
			seen := false
			for _, message := range messages {
				if strings.Contains(message.Content, "same-process-complete") && strings.Contains(message.Content, "Shell command ") {
					seen = true
				}
			}
			completionSeen <- seen
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: "completion follow-up received"}, chat.FinishReasonStop)}}, nil
		}
	}}
	workingDir := t.TempDir()
	manager := shellprocess.New()
	shellRaw, err := builtinshell.CreateToolSet(t.Context(), latest.Toolset{Type: "shell", BackgroundAfter: new(1)}, &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}})
	require.NoError(t, err)
	shellTool, ok := tools.As[*builtinshell.ToolSet](shellRaw)
	require.True(t, ok)
	shellTool.AttachProcessManager(manager, true)
	// Keep the runtime-owned raw job manager alive through team teardown.
	root := agent.New("root", "run shell and report its completion", agent.WithModel(model), agent.WithTitleModel(titleTestProvider{calls: &atomic.Int32{}}), agent.WithToolSets(shellTool))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root), team.WithOwnedResources(manager)), runtime.WithSessionStore(session.NewInMemorySessionStore()), runtime.WithModelStore(execWorkflowModelStore{}), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("auto-shell-exec"), session.WithAgentName("root"), session.WithToolsApproved(true)), runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	var output bytes.Buffer
	flags := &runExecFlags{exec: true}
	err = flags.handleExecMode(t.Context(), cli.NewPrinter(&output), rt, owner.Runtime(), session.New(session.WithID("auto-shell-exec"), session.WithAgentName("root"), session.WithToolsApproved(true)), []string{"mock-team", "run"}, handle)
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 120*time.Millisecond, "exec waits for the same long-running shell process")
	select {
	case seen := <-completionSeen:
		require.True(t, seen, "completion guidance is consumed by a follow-up turn")
	case <-time.After(time.Second):
		t.Fatal("completion follow-up was not run")
	}
	require.Contains(t, output.String(), "completion follow-up received")
	require.EqualValues(t, 3, calls.Load())
}

func TestExecWorkflowWaitsForChildAutomaticShellJobAndFollowups(t *testing.T) {
	rootCalls, workerCalls := atomic.Int32{}, atomic.Int32{}
	childCompletionSeen := make(chan bool, 1)
	rootModel := execBackgroundProvider{call: func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		switch rootCalls.Add(1) {
		case 1:
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "spawn-child", Type: "function", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"worker","task":"run the shell job"}`}}}}, chat.FinishReasonToolCalls)}}, nil
		case 2:
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: "root initial response"}, chat.FinishReasonStop)}}, nil
		default:
			found := false
			for _, message := range messages {
				if strings.Contains(message.Content, "child shell completion received") {
					found = true
				}
			}
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: fmt.Sprintf("root child report received=%v", found)}, chat.FinishReasonStop)}}, nil
		}
	}}
	workerModel := execBackgroundProvider{call: func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		switch workerCalls.Add(1) {
		case 1:
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "child-shell", Type: "function", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"sleep 1.2; echo child-job-finished","timeout":5}`}}}}, chat.FinishReasonToolCalls)}}, nil
		default:
			found := false
			for _, message := range messages {
				if strings.Contains(message.Content, "child-job-finished") && strings.Contains(message.Content, "Shell command ") {
					found = true
				}
			}
			if found {
				select {
				case childCompletionSeen <- true:
				default:
				}
			}
			content := "child shell still running"
			if found {
				content = "child shell completion received"
			}
			return &execWorkflowStream{chunks: []chat.MessageStreamResponse{execWorkflowResponse(chat.MessageDelta{Content: content}, chat.FinishReasonStop)}}, nil
		}
	}}
	workingDir := t.TempDir()
	manager := shellprocess.New()
	shellRaw, err := builtinshell.CreateToolSet(t.Context(), latest.Toolset{Type: "shell", BackgroundAfter: new(1)}, &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}})
	require.NoError(t, err)
	shellTool, ok := tools.As[*builtinshell.ToolSet](shellRaw)
	require.True(t, ok)
	shellTool.AttachProcessManager(manager, true)
	root := agent.New("root", "spawn child and wait", agent.WithModel(rootModel), agent.WithTitleModel(titleTestProvider{calls: &atomic.Int32{}}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}), agent.WithToolSets(subagent.NewToolSet()))
	worker := agent.New("worker", "run shell", agent.WithModel(workerModel), agent.WithTitleModel(titleTestProvider{calls: &atomic.Int32{}}), agent.WithToolSets(shellTool))
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker), team.WithOwnedResources(manager)), runtime.WithSessionStore(session.NewInMemorySessionStore()), runtime.WithModelStore(execWorkflowModelStore{}), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	defer func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }()
	sess := session.New(session.WithID("auto-shell-child-exec"), session.WithAgentName("root"), session.WithToolsApproved(true))
	handle, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	var output bytes.Buffer
	flags := &runExecFlags{exec: true}
	err = flags.handleExecMode(t.Context(), cli.NewPrinter(&output), rt, owner.Runtime(), sess, []string{"mock-team", "do work"}, handle)
	require.NoError(t, err)
	select {
	case seen := <-childCompletionSeen:
		require.True(t, seen, "child's completion guidance reaches its own safe boundary")
	case <-time.After(time.Second):
		t.Fatal("child completion was not observed")
	}
	require.Contains(t, output.String(), "root child report received=true")
	require.GreaterOrEqual(t, rootCalls.Load(), int32(3))
	require.GreaterOrEqual(t, workerCalls.Load(), int32(2))
}
