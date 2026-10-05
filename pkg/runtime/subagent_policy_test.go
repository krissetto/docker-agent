package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

func isolateSubagentPolicyTest(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
}

func policyRuntime(t *testing.T, opts ...Opt) *LocalRuntime {
	t.Helper()
	isolateSubagentPolicyTest(t)
	worker := agent.New("worker", "worker prompt", agent.WithModel(coordinationReply("done")))
	root := agent.New("root", "keep this human instruction about spawn_subagent", agent.WithModel(coordinationReply("root done")),
		agent.WithSubAgents(worker), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}),
		agent.WithAsyncHarnessPrompt(subagent.HarnessPrompt([]subagent.AllowedSubagent{{Agent: "worker"}})),
		agent.WithToolSets(transfertask.New(), agenttool.New()))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), append([]Opt{
		WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}), WithSessionCompaction(false),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt
}

func policyCall(name, args string) tools.ToolCall {
	return tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: args}}
}

func TestUseSubagentsDefaultAndToggle(t *testing.T) {
	var zero LocalRuntime
	require.True(t, zero.UseSubagents())
	zero.SetUseSubagents(false)
	require.False(t, zero.UseSubagents())
	zero.SetUseSubagents(true)
	require.True(t, zero.UseSubagents())
	rt := policyRuntime(t, WithUseSubagents(false))
	require.False(t, rt.UseSubagents())
	parent := session.New(session.WithTitle("parent"))
	_, err := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "worker"}, "denied")
	require.ErrorIs(t, err, errSubagentsDisabled)
	require.Empty(t, rt.subagents.Tree().Snapshot().Nodes)
	rt.SetUseSubagents(true)
	id, err := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "worker"}, "accepted")
	require.NoError(t, err)
	require.NotEmpty(t, id)
}

func TestUseSubagentsRejectsAlreadyStreamedCalls(t *testing.T) {
	rt := policyRuntime(t)
	parent := session.New(session.WithTitle("parent"))
	calls := []tools.ToolCall{
		policyCall(subagent.ToolSpawnSubagent, `{"agent":"worker","task":"do it"}`),
		policyCall(transfertask.ToolNameTransferTask, `{"agent":"worker","task":"do it"}`),
		policyCall(agenttool.ToolNameRunBackgroundAgent, `{"agent":"worker","task":"do it"}`),
	}
	// Calls have already been generated under the old tool exposure.
	rt.SetUseSubagents(false)
	for _, call := range calls {
		result, err := rt.toolMap[call.Function.Name](t.Context(), parent, call, nil, tools.NopRuntime{})
		require.NoError(t, err)
		require.True(t, result.IsError, call.Function.Name)
		assert.Contains(t, result.Output, "disabled")
	}
	result := rt.RunAgent(t.Context(), agenttool.RunParams{ParentSession: parent, AgentName: "worker", Task: "do it"})
	assert.Contains(t, result.ErrMsg, "disabled")
	assert.Empty(t, rt.subagents.Tree().Snapshot().Nodes)
}

func TestUseSubagentsExistingChildRoutes(t *testing.T) {
	isolateSubagentPolicyTest(t)
	m := newTestSubagentManager(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID))
	m.registerChild(parent, "root", "aaaaa", "worker", child)
	_, err := m.sendToChild(parent.ID, "aaaaa", "accepted before off")
	require.NoError(t, err)
	m.r.SetUseSubagents(false)
	require.Equal(t, "accepted before off", requireOneDriverMessage(t, m.r, child))
	require.NoError(t, m.admitChildRun("aaaaa"), "accepted/manual work must not be denied by the common start gate")
	_, err = m.sendToChild(parent.ID, "aaaaa", "new work while running")
	require.ErrorIs(t, err, errSubagentsDisabled)
	m.children["aaaaa"].durable.Result = "accepted task completed"
	m.reportTurn(t, "aaaaa", subagent.NodeIdle, "")
	assert.Contains(t, requireOneDriverMessage(t, m.r, parent), "accepted task completed")
	_, err = m.sendToChild(parent.ID, "aaaaa", "new idle wakeup")
	require.ErrorIs(t, err, errSubagentsDisabled)
	require.Empty(t, drainDriverMessages(m.r, child))
	require.True(t, m.deliverExplicitToParent(parent.ID, "parent update", child.ID, "worker"))
	assert.Equal(t, "parent update", requireOneDriverMessage(t, m.r, parent))
	_, err = m.readChild(parent.ID, "aaaaa")
	require.NoError(t, err)
	_, err = m.stopChild(parent.ID, "aaaaa")
	require.NoError(t, err)
	m.r.SetUseSubagents(true)
	_, err = m.sendToChild(parent.ID, "aaaaa", "must not revive")
	require.ErrorContains(t, err, "stopped")
}

func TestUseSubagentsFiltersDynamicExposure(t *testing.T) {
	rt := policyRuntime(t)
	a, err := rt.team.Agent("root")
	require.NoError(t, err)
	parent := session.New(session.WithID(t.Name() + "/parent"))
	rt.subagents.ensureRoot(parent, a.Name())
	sess := session.New(session.WithParentID(parent.ID), session.WithAsyncSubagent(true))
	sess.AddMessage(session.UserMessage("user says spawn_subagent"))
	all := append(subagent.Definitions(), tools.Tool{Name: agenttool.ToolNameRunBackgroundAgent}, tools.Tool{Name: transfertask.ToolNameTransferTask})
	rt.SetUseSubagents(false)
	filtered := rt.filterDelegationTools(addAsyncChildTools(sess, all))
	var names []string
	for _, tool := range filtered {
		names = append(names, tool.Name)
	}
	assert.NotContains(t, names, subagent.ToolSpawnSubagent)
	assert.NotContains(t, names, agenttool.ToolNameRunBackgroundAgent)
	assert.NotContains(t, names, transfertask.ToolNameTransferTask)
	assert.Contains(t, names, subagent.ToolSendMessage)
	assert.Contains(t, names, subagent.ToolReadSubagent)
	assert.Contains(t, names, subagent.ToolStopSubagent)
	messages := rt.filterDelegationMessages(a, sess, sess.GetMessages(a))
	var text strings.Builder
	for _, msg := range messages {
		text.WriteString(msg.Content)
	}
	assert.NotContains(t, text.String(), "Your subagents (use these names")
	assert.NotContains(t, text.String(), "Use transfer_task only")
	assert.NotContains(t, text.String(), "Use background agent tasks to dispatch")
	assert.Contains(t, text.String(), a.Instruction())
	assert.Contains(t, text.String(), "user says spawn_subagent")
	assert.Contains(t, text.String(), subagent.ChildInstructions())
	assert.Contains(t, text.String(), "contact your parent")
	rt.SetUseSubagents(true)
	assert.Equal(t, all, rt.filterDelegationTools(all))
	assert.Equal(t, sess.GetMessages(a), rt.filterDelegationMessages(a, sess, sess.GetMessages(a)))
}

func TestUseSubagentsRaceAdmissionAndSetter(t *testing.T) {
	rt := policyRuntime(t, WithMaxActiveDescendants(100), WithMaxActiveDescendantsPerRoot(100))
	parent := session.New(session.WithTitle("parent"))
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			_, err := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "worker"}, fmt.Sprintf("task %d", i))
			if err != nil {
				assert.ErrorIs(t, err, errSubagentsDisabled)
			}
		})
	}
	wg.Go(func() { rt.SetUseSubagents(false) })
	wg.Wait()
	for range 10 {
		_, err := rt.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "worker"}, "after setter")
		require.ErrorIs(t, err, errSubagentsDisabled)
	}
	for range 8 {
		wg.Go(func() {
			for i := range 100 {
				rt.SetUseSubagents(i%2 == 0)
				_ = rt.UseSubagents()
				_ = rt.filterDelegationTools(subagent.Definitions())
			}
		})
	}
	wg.Wait()
}

func TestUseSubagentsOffPreservesRunningAndManualChild(t *testing.T) {
	isolateSubagentPolicyTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	worker := coordinationReply("done")
	var once sync.Once
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("done after off").AddStopWithUsage(1, 1).Build(), nil
	}
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root done"), worker)
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	// Manual creation uses the same durable child-admission transaction.
	rt.SetUseSubagents(false)
	child := coordinationCreate(t, owner.Runtime(), "manual-child", parent.ID())
	rt.SetUseSubagents(true)
	rootSession, err := parent.Snapshot(t.Context())
	require.NoError(t, err)
	id, err := rt.subagents.Spawn(rootSession, "root", subagent.AllowedSubagent{Agent: "worker"}, "accepted task")
	require.NoError(t, err)
	coordinationWait(t, entered)
	rt.SetUseSubagents(false)
	close(release)
	require.Eventually(t, func() bool {
		rec, ok := rt.subagents.Read(id)
		return ok && rec.state == subagent.NodeIdle && rec.result == "done after off"
	}, 5*time.Second, time.Millisecond)
	submission, err := child.Submit(t.Context(), TurnInput{Content: "manual work while disabled"})
	require.NoError(t, err)
	coordinationAwait(t, child, submission.TurnID)
	snapshot, err := child.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "done after off", snapshot.GetLastAssistantMessageContent())
}

type policyProvider struct {
	*mockProvider
	call func(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error)
}

func (p *policyProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
	return p.call(ctx, messages, available)
}

func TestUseSubagentsProviderInFlightAndReenable(t *testing.T) {
	isolateSubagentPolicyTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var mu sync.Mutex
	seen := map[string]bool{}
	var captures [][]tools.Tool
	model := &policyProvider{mockProvider: &mockProvider{id: "test/policy"}}
	model.call = func(ctx context.Context, messages []chat.Message, available []tools.Tool) (chat.MessageStream, error) {
		request := ""
		for _, message := range messages {
			if message.Role == chat.MessageRoleUser && (message.Content == "in-flight spawn" || message.Content == "hidden spawn" || message.Content == "reenabled spawn") {
				request = message.Content
			}
		}
		mu.Lock()
		first := request != "" && !seen[request]
		seen[request] = true
		if first {
			captures = append(captures, available)
		}
		mu.Unlock()
		if !first {
			return newStreamBuilder().AddContent("finished").AddStopWithUsage(1, 1).Build(), nil
		}
		if request == "in-flight spawn" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddToolCallName(request, subagent.ToolSpawnSubagent).
			AddToolCallArguments(request, `{"agent":"worker","task":"do it"}`).AddToolCallStopWithUsage(1, 1).Build(), nil
	}
	root := agent.New("root", "prompt", agent.WithModel(model), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}),
		agent.WithToolSets(subagent.NewToolSet()))
	worker := agent.New("worker", "prompt", agent.WithModel(coordinationReply("worker done")))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithSessionStore(session.NewInMemorySessionStore()), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("policy-root"), session.WithTitle("Policy"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := h.Submit(t.Context(), TurnInput{Content: "in-flight spawn"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	rt.SetUseSubagents(false)
	releaseOnce.Do(func() { close(release) })
	coordinationAwait(t, h, first.TurnID)
	coordinationSettled(t, h)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Contains(t, renderTranscript(snapshot, 0), "delegation is disabled")
	assert.Empty(t, rt.subagents.children)

	hidden, err := h.Submit(t.Context(), TurnInput{Content: "hidden spawn"})
	require.NoError(t, err)
	coordinationAwait(t, h, hidden.TurnID)
	coordinationSettled(t, h)
	assert.Empty(t, rt.subagents.children)

	rt.SetUseSubagents(true)
	last, err := h.Submit(t.Context(), TurnInput{Content: "reenabled spawn"})
	require.NoError(t, err)
	coordinationAwait(t, h, last.TurnID)
	rt.subagents.mu.Lock()
	assert.Len(t, rt.subagents.children, 1)
	rt.subagents.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, captures, 3)
	for i, available := range captures {
		found := false
		for _, tool := range available {
			found = found || tool.Name == subagent.ToolSpawnSubagent
		}
		assert.Equal(t, i != 1, found, "provider-facing spawn exposure on request %d", i)
	}
}
