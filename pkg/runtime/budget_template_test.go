package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

func TestBudgetTemplatesDetachCallerOwnedMapsAndSlices(t *testing.T) {
	cfg := &latest.BudgetConfig{MaxTokens: 10}
	defs := map[string]latest.BudgetConfig{"shared": {MaxTokens: 20}}
	names := map[string][]string{"worker": {"shared"}}
	r := &LocalRuntime{}
	WithBudget(cfg)(r)
	WithNamedBudgets(defs, names)(r)
	cfg.MaxTokens = 0
	defs["shared"] = latest.BudgetConfig{}
	names["worker"][0] = "missing"
	delete(names, "worker")
	root := session.New()
	child := session.New(session.WithParentID(root.ID))
	wallet := r.rootBudget(root)
	require.NotNil(t, wallet)
	require.Same(t, wallet, r.rootBudget(child))
	targets := wallet.budgetsFor("worker")
	require.Len(t, targets, 2)
	require.Equal(t, int64(10), targets[0].Tracker.maxTokens)
	require.Equal(t, int64(20), targets[1].Tracker.maxTokens)
	second := r.rootBudget(session.New())
	require.NotSame(t, wallet, second)
	targets[1].Tracker.record("worker", &chat.Usage{InputTokens: 20}, nil, 0)
	require.NotNil(t, wallet.exceededFor("worker"))
	require.Nil(t, second.exceededFor("worker"))
}

func TestExistingBudgetWalletDoesNotDependOnCurrentTemplate(t *testing.T) {
	r := &LocalRuntime{}
	WithBudget(&latest.BudgetConfig{MaxTokens: 10})(r)
	root := session.New()
	wallet := r.rootBudget(root)
	wallet.budgetsFor("root")[0].Tracker.record("root", &chat.Usage{InputTokens: 10}, nil, 0)
	r.budgetCfg = nil
	r.budgetsCfg = nil
	require.Same(t, wallet, r.rootBudget(root))
	require.NotNil(t, r.rootBudget(root).exceededFor("root"))
	require.Nil(t, r.rootBudget(session.New()))
}
