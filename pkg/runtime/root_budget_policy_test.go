package runtime

import (
	"context"
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

func TestRootBudgetSharesDescendantsAndIsolatesRoots(t *testing.T) {
	m := newTestSubagentManager(t)
	r := m.r
	WithBudget(&latest.BudgetConfig{MaxTokens: 10})(r)
	root := session.New(session.WithID("root-one"))
	other := session.New(session.WithID("root-two"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAsyncSubagent(true))
	m.registerChild(root, "root", "aaaaa", "worker", child)
	require.Same(t, r.rootBudget(root), r.rootBudget(child))
	require.NotSame(t, r.rootBudget(root), r.rootBudget(other))
	r.recordBudget(child, agent.New("worker", ""), &chat.Usage{InputTokens: 10}, nil, 0, &collectSink{})
	require.NotNil(t, r.rootBudget(root).exceededFor("root"))
	assert.Nil(t, r.rootBudget(other).exceededFor("root"))
}

func TestRootElapsedBudgetCancelsWaitWithoutUsage(t *testing.T) {
	r := &LocalRuntime{}
	WithBudget(&latest.BudgetConfig{MaxTime: latest.Duration{Duration: 20 * time.Millisecond}})(r)
	sess := session.New()
	ctx, cancel := r.budgetContext(t.Context(), sess, agent.New("root", ""))
	defer cancel()
	select {
	case <-ctx.Done():
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("elapsed budget did not cancel a blocked operation")
	}
	require.NotNil(t, r.rootBudget(sess).exceededFor("root"))
}

func TestCanonicalDelegationCannotBypassZeroLimits(t *testing.T) {
	for _, limit := range []string{"subagent_depth", "active_descendants"} {
		t.Run(limit, func(t *testing.T) {
			for _, route := range []string{"sync", "transfer", "background", "async"} {
				t.Run(route, func(t *testing.T) {
					r := policyRuntime(t)
					if limit == "subagent_depth" {
						r.maxSubagentDepth = 0
					} else {
						r.maxActiveDescendants = 0
					}
					parent := session.New()
					switch route {
					case "sync":
						result := r.RunAgent(t.Context(), agenttool.RunParams{ParentSession: parent, AgentName: "worker", Task: "do work"})
						assert.Contains(t, result.ErrMsg, limit)
					case "transfer":
						result, err := r.handleTaskTransfer(t.Context(), parent, transferToolCall("worker"), &collectSink{}, tools.NopRuntime{})
						require.NoError(t, err)
						require.True(t, result.IsError)
						assert.Contains(t, result.Output, limit)
					case "background":
						result, err := r.handleBackgroundRun(t.Context(), parent, policyCall(agenttool.ToolNameRunBackgroundAgent, `{"agent":"worker","task":"do work"}`), &collectSink{}, tools.NopRuntime{})
						require.NoError(t, err)
						require.True(t, result.IsError)
						assert.Contains(t, result.Output, limit)
					case "async":
						_, err := r.subagents.Spawn(parent, "root", subagent.AllowedSubagent{Agent: "worker"}, "do work")
						require.ErrorContains(t, err, limit)
					}
				})
			}
		})
	}
}

func TestBackgroundControlsCannotCrossRoots(t *testing.T) {
	m := newTestSubagentManager(t)
	first, second := session.New(session.WithID("first")), session.New(session.WithID("second"))
	child := session.New(session.WithID("child"), session.WithParentID(first.ID), session.WithAsyncSubagent(true))
	m.registerChild(first, "root", "aaaaa", "worker", child)
	_, err := m.readChild(second.ID, "aaaaa")
	require.Error(t, err)
	_, err = m.stopChild(second.ID, "aaaaa")
	require.Error(t, err)
	result, err := m.r.handleBackgroundList(t.Context(), second, tools.ToolCall{}, &collectSink{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.NotContains(t, result.Output, "aaaaa")
}

func TestSynchronousNestedDelegationProgressesWithSingleExecution(t *testing.T) {
	helper := agent.New("helper", "", agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("helper done").AddStopWithUsage(1, 1).Build()}))
	worker := agent.New("worker", "", agent.WithModel(&queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallName("nested", "transfer_task").AddToolCallArguments("nested", `{"agent":"helper","task":"finish"}`).AddToolCallStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("worker done").AddStopWithUsage(1, 1).Build(),
	}}), agent.WithSubAgents(helper), agent.WithToolSets(transfertask.New()))
	root := agent.New("root", "", agent.WithModel(&queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallName("child", "transfer_task").AddToolCallArguments("child", `{"agent":"worker","task":"finish"}`).AddToolCallStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}}), agent.WithSubAgents(worker), agent.WithToolSets(transfertask.New()))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker, helper)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	r.policy.MaxExecutions = 1
	r.policy.MaxTools = 1
	r.applyResourcePolicy()
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sess := session.New(session.WithToolsApproved(true), session.WithNonInteractive(true), session.WithUserMessage("begin"))
	for event := range r.runExecution(ctx, sess) {
		if failure, ok := event.(*ErrorEvent); ok {
			t.Fatalf("nested execution failed: %s", failure.Error)
		}
	}
	require.NoError(t, ctx.Err(), "nested delegation deadlocked execution capacity")
	driver, ok := r.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	assert.Equal(t, "root done", driver.session().GetLastAssistantMessageContent())
	require.Len(t, r.subagents.children, 2)
}

func TestBudgetCountsCachedPromptTokens(t *testing.T) {
	tracker := newBudgetTracker(&latest.BudgetConfig{MaxTokens: 10})
	tracker.record("root", &chat.Usage{InputTokens: 2, CachedInputTokens: 3, CacheWriteTokens: 4, OutputTokens: 1}, nil, 0)
	require.Equal(t, int64(10), tracker.snapshot().Tokens)
	require.NotNil(t, tracker.exceeded())
}

func TestSharedBudgetExhaustionCancelsActiveDescendantsOnly(t *testing.T) {
	m := newTestSubagentManager(t)
	r := m.r
	WithBudget(&latest.BudgetConfig{MaxTokens: 2})(r)
	root := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAsyncSubagent(true))
	other := session.New(session.WithID("other"))
	m.registerChild(root, "root", "aaaaa", "worker", child)
	a := agent.New("worker", "")
	childCtx, cancelChild := r.budgetContext(t.Context(), child, a)
	defer cancelChild()
	otherCtx, cancelOther := r.budgetContext(t.Context(), other, a)
	defer cancelOther()
	r.recordBudget(root, a, &chat.Usage{InputTokens: 2}, nil, 0, &collectSink{})
	select {
	case <-childCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("shared exhausted budget did not cancel child")
	}
	assert.NoError(t, otherCtx.Err())
}

type budgetHangingProvider struct{ *mockProvider }

func (p *budgetHangingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRootElapsedBudgetCancelsHangingProvider(t *testing.T) {
	worker := agent.New("worker", "", agent.WithModel(&budgetHangingProvider{mockProvider: &mockProvider{id: "test/mock-model"}}))
	root := agent.New("root", "", agent.WithModel(coordinationReply("done")), agent.WithSubAgents(worker))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithModelStore(mockModelStore{}), WithSessionCompaction(false), WithBudget(&latest.BudgetConfig{MaxTime: latest.Duration{Duration: 20 * time.Millisecond}}))
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	parent := session.New()
	r.RunAgent(ctx, agenttool.RunParams{ParentSession: parent, AgentName: "worker", Task: "blocked provider"})
	require.NoError(t, ctx.Err(), "outer timeout fired instead of root elapsed budget")
	require.NotNil(t, r.rootBudget(parent).exceededFor("worker"))
}
