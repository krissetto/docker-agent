package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func budgetRevisionEnvironment(t *testing.T, revision string, policy BudgetPolicyConfig) *TeamEnvironment {
	t.Helper()
	tm := team.New(team.WithAgents(agent.New("root", "instructions")))
	env, err := NewTeamEnvironment(revision, "budget-source", "budget-baseline", "budget-team", tm, ModelSwitcherConfig{}, func(context.Context) error { return nil }, WithTeamEnvironmentBudgetConfig(policy))
	require.NoError(t, err)
	return env
}

func TestRevisionBudgetViewsShareLedgerButKeepPinnedLimits(t *testing.T) {
	t.Parallel()
	oldEnv := budgetRevisionEnvironment(t, "budget-r1", BudgetPolicyConfig{Run: &latest.BudgetConfig{MaxTokens: 100}, Named: map[string]latest.BudgetConfig{"shared": {MaxTokens: 100}}, AgentBudgets: map[string][]string{"root": {"shared"}}})
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	oldLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	r := &LocalRuntime{teamEnvironments: store}
	sess := session.New(session.WithID("budget-root"), session.WithAgentName("root"))
	oldCtx := environmentScopedContext(t.Context(), oldLease)
	root := agent.New("root", "instructions")

	r.recordBudgetForContext(oldCtx, sess, root, &chat.Usage{InputTokens: 7}, nil, time.Second, &collectSink{})
	require.Nil(t, r.budgetViewForContext(oldCtx, sess).exceededFor("root"))

	// A run pinned to r1 captures and retains r1's higher limit.
	oldRunCtx, cancelOld := r.executionBudgetContext(oldCtx, sess, root)
	defer cancelOld()
	newEnv := budgetRevisionEnvironment(t, "budget-r2", BudgetPolicyConfig{Run: &latest.BudgetConfig{MaxTokens: 10}, Named: map[string]latest.BudgetConfig{"shared": {MaxTokens: 10}}, AgentBudgets: map[string][]string{"root": {"shared"}}})
	require.NoError(t, store.Publish("budget-source", "budget-r1", newEnv))
	newLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	newCtx := environmentScopedContext(t.Context(), newLease)

	r.recordBudgetForContext(oldRunCtx, sess, root, &chat.Usage{InputTokens: 4}, nil, time.Second, &collectSink{})
	require.Nil(t, r.budgetViewForContext(oldRunCtx, sess).exceededFor("root"), "an old in-flight operation keeps its old limit")
	require.NotNil(t, r.budgetViewForContext(newCtx, sess).exceededFor("root"), "a new operation sees tightened limit and accumulated spend")
	usage := r.budgetUsageForContext(newCtx, sess, "root")
	require.Len(t, usage, 2)
	require.Equal(t, int64(11), usage[0].Snapshot.Tokens)
	require.Equal(t, int64(10), usage[0].Snapshot.MaxTokens)
	require.Equal(t, int64(11), usage[1].Snapshot.Tokens)

	require.NoError(t, newLease.Release(t.Context()))
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestRevisionBudgetAccountsSpendBeforeLimitAddedAndKeepRemovedPots(t *testing.T) {
	t.Parallel()
	baseline := budgetRevisionEnvironment(t, "budget-empty", BudgetPolicyConfig{})
	store, err := NewTeamEnvironmentStore(baseline)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	r := &LocalRuntime{teamEnvironments: store}
	sess := session.New(session.WithID("budget-add"), session.WithAgentName("root"))
	root := agent.New("root", "instructions")
	oldCtx := environmentScopedContext(t.Context(), lease)
	// Revision-mode accounting always retains root spend, even with no active cap.
	r.recordBudgetForContext(oldCtx, sess, root, &chat.Usage{InputTokens: 3}, nil, time.Second, &collectSink{})
	noLimitEnv := budgetRevisionEnvironment(t, "budget-named-removed", BudgetPolicyConfig{Named: map[string]latest.BudgetConfig{"pot": {}}, AgentBudgets: map[string][]string{"root": {"pot"}}})
	require.NoError(t, store.Publish("budget-source", "budget-empty", noLimitEnv))
	lease2, err := store.AcquireCurrent()
	require.NoError(t, err)
	ctx2 := environmentScopedContext(t.Context(), lease2)
	r.recordBudgetForContext(ctx2, sess, root, &chat.Usage{InputTokens: 5}, nil, time.Second, &collectSink{})
	require.NoError(t, store.Publish("budget-source", "budget-named-removed", budgetRevisionEnvironment(t, "budget-readded", BudgetPolicyConfig{Run: &latest.BudgetConfig{MaxTokens: 4}, Named: map[string]latest.BudgetConfig{"pot": {MaxTokens: 4}}, AgentBudgets: map[string][]string{"root": {"pot"}}})))
	lease3, err := store.AcquireCurrent()
	require.NoError(t, err)
	ctx3 := environmentScopedContext(t.Context(), lease3)
	view := r.budgetViewForContext(ctx3, sess)
	require.NotNil(t, view.exceededFor("root"), "new limits inspect run and named pot spend accumulated before and while those limits were absent")
	usage := view.snapshot()
	require.Len(t, usage, 2)
	require.Equal(t, int64(8), usage[0].Snapshot.Tokens, "run spend before a limit was added is retained")
	require.Equal(t, int64(5), usage[1].Snapshot.Tokens, "a named pot retains all spend attributed to that pot across removal/re-addition")

	require.NoError(t, lease3.Release(t.Context()))
	require.NoError(t, lease2.Release(t.Context()))
	require.NoError(t, lease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestRevisionBudgetPolicyCopiesInputAndAccountsUnknownAgentsToRun(t *testing.T) {
	t.Parallel()
	run := latest.BudgetConfig{MaxTokens: 2}
	agentBudgets := map[string][]string{"root": {"named"}}
	named := map[string]latest.BudgetConfig{"named": {MaxTokens: 1}}
	env := budgetRevisionEnvironment(t, "copy", BudgetPolicyConfig{Run: &run, Named: named, AgentBudgets: agentBudgets})
	run.MaxTokens = 0
	named["named"] = latest.BudgetConfig{}
	agentBudgets["root"][0] = "other"
	policy := env.BudgetPolicy()
	require.Equal(t, int64(2), policy.Run.MaxTokens)
	require.Equal(t, int64(1), policy.Named["named"].MaxTokens)
	require.Equal(t, []string{"named"}, policy.AgentBudgets["root"])
	policy.AgentBudgets["root"][0] = "mutated"
	require.Equal(t, []string{"named"}, env.BudgetPolicy().AgentBudgets["root"])

	store, err := NewTeamEnvironmentStore(env)
	require.NoError(t, err)
	lease, err := store.AcquireCurrent()
	require.NoError(t, err)
	r := &LocalRuntime{teamEnvironments: store}
	sess := session.New(session.WithID("budget-unknown"), session.WithAgentName("new-name"))
	ctx := environmentScopedContext(t.Context(), lease)
	r.recordBudgetForContext(ctx, sess, agent.New("new-name", "instructions"), &chat.Usage{InputTokens: 3}, nil, time.Second, &collectSink{})
	view := r.budgetViewForContext(ctx, sess)
	require.NotNil(t, view.exceededFor("new-name"), "undeclared agent still counts against the run-wide cap")
	usage := view.snapshot()
	require.Len(t, usage, 2)
	require.Equal(t, int64(3), usage[0].Snapshot.Tokens)
	require.Equal(t, int64(0), usage[1].Snapshot.Tokens, "unknown agent does not spend a named pot it was not assigned")
	require.NoError(t, lease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}

func TestExecutionBudgetBindingsKeepTheirPinnedRevisionPolicy(t *testing.T) {
	t.Parallel()
	oldEnv := budgetRevisionEnvironment(t, "binding-r1", BudgetPolicyConfig{Run: &latest.BudgetConfig{MaxTokens: 100}})
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	oldLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	r := &LocalRuntime{teamEnvironments: store}
	sess := session.New(session.WithID("binding-root"), session.WithAgentName("root"))
	root := agent.New("root", "instructions")
	oldCtx, cancelOld := r.executionBudgetContext(environmentScopedContext(t.Context(), oldLease), sess, root)
	defer cancelOld()
	oldBinding, ok := oldCtx.Value(executionBudgetKey{}).(*executionBudgetBinding)
	require.True(t, ok)
	require.Equal(t, int64(100), oldBinding.policy.limits[runBudgetName].MaxTokens)

	newEnv := budgetRevisionEnvironment(t, "binding-r2", BudgetPolicyConfig{Run: &latest.BudgetConfig{MaxTokens: 10}})
	require.NoError(t, store.Publish("budget-source", "binding-r1", newEnv))
	newLease, err := store.AcquireCurrent()
	require.NoError(t, err)
	newCtx, cancelNew := r.executionBudgetContext(environmentScopedContext(t.Context(), newLease), sess, root)
	defer cancelNew()
	newBinding, ok := newCtx.Value(executionBudgetKey{}).(*executionBudgetBinding)
	require.True(t, ok)
	require.Equal(t, int64(10), newBinding.policy.limits[runBudgetName].MaxTokens)
	require.Same(t, oldBinding.wallet, newBinding.wallet, "revision bindings share usage ledger")

	r.recordBudgetForContext(newCtx, sess, root, &chat.Usage{InputTokens: 11}, nil, 0, &collectSink{})
	select {
	case <-newCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("new revision's tightened policy should cancel its execution")
	}
	select {
	case <-oldCtx.Done():
		t.Fatal("new revision limit must not cancel an in-flight operation pinned to the old limit")
	default:
	}

	require.NoError(t, newLease.Release(t.Context()))
	require.NoError(t, oldLease.Release(t.Context()))
	require.NoError(t, store.Close(t.Context()))
}
