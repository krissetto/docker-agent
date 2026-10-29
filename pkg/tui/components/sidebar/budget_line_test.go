package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
)

func (s *testSidebar) feedBudget(ev *runtime.BudgetUsageEvent) {
	updated, _ := s.Update(ev)
	s.model = updated.(*model)
}

func TestBudgetLine_AbsentWhenUnbudgeted(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	m.recordUsageTokens("s1", "root", 5000, 3000)

	assert.NotContains(t, ansi.Strip(m.tokenUsage(60)), "/$")
}

func TestBudgetLine_ShowsCeilingsAtZero(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets: []runtime.BudgetStatus{{
			Name: "run", MaxCost: 0.50, MaxTokens: 100000, MaxTimeSeconds: 120,
		}},
	})

	out := ansi.Strip(m.tokenUsage(60))
	assert.Contains(t, out, "run")
	assert.Contains(t, out, "$0.00/$0.50")
	assert.Contains(t, out, "/100.0K")
	assert.Contains(t, out, "/2m", "a round ceiling reads 2m, not 2m0s")
}

func TestFormatBudgetDuration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		seconds float64
		want    string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{134, "2m14s"},
		{600, "10m"},
		{3600, "1h"},
		{5400, "1h30m"},
	} {
		assert.Equal(t, tc.want, formatBudgetDuration(tc.seconds))
	}
}

func TestBudgetLine_ShowsEachBudgetByName(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Budgets: []runtime.BudgetStatus{
			{Name: "run", Cost: 0.12, MaxCost: 0.50},
			{Name: "shell-work", Cost: 0.09, MaxCost: 0.10},
		},
	})

	out := ansi.Strip(m.tokenUsage(80))
	assert.Contains(t, out, "run")
	assert.Contains(t, out, "$0.12/$0.50")
	assert.Contains(t, out, "shell-work")
	assert.Contains(t, out, "$0.09/$0.10")
	assert.Less(t, strings.Index(out, "run"), strings.Index(out, "shell-work"),
		"run-wide budget leads, then named budgets")
}

func TestBudgetLine_OmitsPerAgentBreakdown(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Budgets: []runtime.BudgetStatus{{
			Name: "run", Cost: 0.12, MaxCost: 0.50,
			PerAgent: []runtime.AgentBudgetUsage{
				{AgentName: "developer", Cost: 0.09, Tokens: 8000, ActiveSeconds: 72},
				{AgentName: "solo-agent", Cost: 0.03, Tokens: 4300, ActiveSeconds: 62},
			},
		}},
	})

	out := ansi.Strip(m.tokenUsage(80))
	assert.Contains(t, out, "$0.12/$0.50", "the budget total still shows")
	assert.NotContains(t, out, "solo-agent",
		"per-agent rows are the Agents section's job, not the budget line's")
}

func TestBudgetLine_SubCentUsesPreciseFormat(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets:      []runtime.BudgetStatus{{Name: "run", Cost: 0.0012, MaxCost: 0.05}},
	})

	out := ansi.Strip(m.tokenUsage(60))
	assert.Contains(t, out, "$0.0012", "sub-cent spend must not read as $0.00")
}

func TestBudgetLine_TokensOnlyBudgetOmitsCost(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets:      []runtime.BudgetStatus{{Name: "roomy", Tokens: 500, MaxTokens: 1000}},
	})

	var row string
	for line := range strings.SplitSeq(ansi.Strip(m.tokenUsage(80)), "\n") {
		if strings.Contains(line, "roomy") {
			row = line
			break
		}
	}
	assert.NotEmpty(t, row, "the tokens-only budget must be rendered")
	assert.Contains(t, row, "500/1.0K")
	assert.NotContains(t, row, "$", "a tokens-only budget must not render costs")
}

func TestBudgetLine_MarksUnpricedSpend(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID:    "s1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets:      []runtime.BudgetStatus{{Name: "run", Cost: 0, MaxCost: 0.50, Unpriced: true}},
	})

	assert.Contains(t, ansi.Strip(m.tokenUsage(80)), "unpriced spend")
}

func TestBudgetLine_UpdatesLive(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("s1", "root")
	setUsageReadyForTest(m.model)
	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID: "s1", AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets: []runtime.BudgetStatus{{Name: "run", Cost: 0.01, MaxCost: 0.50}},
	})
	assert.Contains(t, ansi.Strip(m.tokenUsage(60)), "$0.01/$0.50")

	m.feedBudget(&runtime.BudgetUsageEvent{
		SessionID: "s1", AgentContext: runtime.AgentContext{AgentName: "root"},
		Budgets: []runtime.BudgetStatus{{Name: "run", Cost: 0.44, MaxCost: 0.50}},
	})
	out := ansi.Strip(m.tokenUsage(60))
	assert.Contains(t, out, "$0.44/$0.50")
	assert.NotContains(t, out, "$0.01/$0.50", "stale reading must be replaced")
}
