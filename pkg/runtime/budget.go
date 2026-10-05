package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

type budgetLimit string

const (
	budgetLimitCost   budgetLimit = "max_cost"
	budgetLimitTokens budgetLimit = "max_tokens"
	budgetLimitTime   budgetLimit = "max_time"
)

type budgetTracker struct {
	maxCost   float64
	maxTokens int64
	maxTime   time.Duration
	mu        sync.Mutex
	cost      float64
	tokens    int64
	active    time.Duration
	started   time.Time
	unpriced  bool
	perAgent  map[string]*agentSpend
}

type agentSpend struct {
	cost   float64
	tokens int64
	active time.Duration
}

func newBudgetTracker(cfg *latest.BudgetConfig) *budgetTracker {
	if cfg.IsZero() {
		return nil
	}
	return &budgetTracker{
		maxCost:   cfg.MaxCost,
		maxTokens: cfg.MaxTokens,
		maxTime:   cfg.MaxTime.Duration,
		perAgent:  make(map[string]*agentSpend),
	}
}

func (b *budgetTracker) record(agentName string, usage *chat.Usage, cost *float64, active time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var addTokens int64
	if usage != nil {
		addTokens = usage.PromptTokens() + usage.OutputTokens
		b.tokens += addTokens
	}
	var addCost float64
	switch {
	case cost != nil:
		addCost = *cost
		b.cost += addCost
	case usage != nil:
		b.unpriced = true
	}

	if active > 0 {
		b.active += active
	}

	spend := b.perAgent[agentName]
	if spend == nil {
		spend = &agentSpend{}
		b.perAgent[agentName] = spend
	}
	spend.cost += addCost
	spend.tokens += addTokens
	if active > 0 {
		spend.active += active
	}
}

type budgetSnapshot struct {
	Cost      float64
	MaxCost   float64
	Tokens    int64
	MaxTokens int64
	Elapsed   time.Duration
	MaxTime   time.Duration
	Unpriced  bool
	PerAgent  []agentBudgetSpend
}

type agentBudgetSpend struct {
	AgentName string
	Cost      float64
	Tokens    int64
	Active    time.Duration
}

func (b *budgetTracker) snapshot() budgetSnapshot {
	if b == nil {
		return budgetSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return budgetSnapshot{
		Cost:      b.cost,
		MaxCost:   b.maxCost,
		Tokens:    b.tokens,
		MaxTokens: b.maxTokens,
		Elapsed:   b.elapsedLocked(),
		MaxTime:   b.maxTime,
		Unpriced:  b.unpriced,
		PerAgent:  b.perAgentSpendLocked(),
	}
}

func (b *budgetTracker) elapsedLocked() time.Duration {
	if !b.started.IsZero() {
		return time.Since(b.started)
	}
	return b.active
}

func (b *budgetTracker) perAgentSpendLocked() []agentBudgetSpend {
	if len(b.perAgent) == 0 {
		return nil
	}
	out := make([]agentBudgetSpend, 0, len(b.perAgent))
	for name, s := range b.perAgent {
		out = append(out, agentBudgetSpend{
			AgentName: name,
			Cost:      s.cost,
			Tokens:    s.tokens,
			Active:    s.active,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].AgentName < out[j].AgentName
	})
	return out
}

type budgetBreach struct {
	Budget string
	Limit  budgetLimit
	Used   string
	Max    string
}

func (br budgetBreach) Message() string {
	return fmt.Sprintf(
		"Execution stopped after reaching the configured %s limit (used %s of %s). Metered usage is reported after calls complete and can exceed the configured limit.",
		br.configPath(), br.Used, br.Max,
	)
}

func (br budgetBreach) configPath() string {
	if br.Budget == "" || br.Budget == runBudgetName {
		return "budget." + string(br.Limit)
	}
	return "budgets." + br.Budget + "." + string(br.Limit)
}

func (b *budgetTracker) exceeded() *budgetBreach {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.maxCost > 0 && b.cost >= b.maxCost {
		return &budgetBreach{
			Limit: budgetLimitCost,
			Used:  formatUSD(b.cost),
			Max:   formatUSD(b.maxCost),
		}
	}
	if b.maxTokens > 0 && b.tokens >= b.maxTokens {
		return &budgetBreach{
			Limit: budgetLimitTokens,
			Used:  fmt.Sprintf("%d tokens", b.tokens),
			Max:   fmt.Sprintf("%d tokens", b.maxTokens),
		}
	}
	if b.maxTime > 0 && b.elapsedLocked() >= b.maxTime {
		return &budgetBreach{
			Limit: budgetLimitTime,
			Used:  b.elapsedLocked().Round(time.Second).String(),
			Max:   b.maxTime.String(),
		}
	}
	return nil
}

func (b *budgetTracker) unpricedSpend() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unpriced && b.maxCost > 0
}

func formatUSD(v float64) string {
	if v < 0 {
		return "-" + formatUSD(-v)
	}
	if v < 0.0001 {
		return "$0.00"
	}
	if v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

const runBudgetName = "run"

type budgetSet struct {
	trackers     map[string]*budgetTracker
	agentBudgets map[string][]string
	order        []string
	contextMu    sync.Mutex
	contexts     map[*context.CancelFunc]string
}

func (s *budgetSet) budgetsFor(agentName string) []namedTracker {
	if s == nil {
		return nil
	}
	names := s.agentBudgets[agentName]
	if len(names) == 0 {
		if t := s.trackers[runBudgetName]; t != nil {
			return []namedTracker{{Name: runBudgetName, Tracker: t}}
		}
		return nil
	}
	out := make([]namedTracker, 0, len(names))
	for _, n := range names {
		if t := s.trackers[n]; t != nil {
			out = append(out, namedTracker{Name: n, Tracker: t})
		}
	}
	return out
}

func (s *budgetSet) all() []namedTracker {
	if s == nil {
		return nil
	}
	out := make([]namedTracker, 0, len(s.order))
	for _, n := range s.order {
		if t := s.trackers[n]; t != nil {
			out = append(out, namedTracker{Name: n, Tracker: t})
		}
	}
	return out
}

type namedTracker struct {
	Name    string
	Tracker *budgetTracker
}

func newBudgetSet(runBudget *latest.BudgetConfig, named map[string]latest.BudgetConfig, agentBudgets map[string][]string) *budgetSet {
	s := &budgetSet{
		trackers:     make(map[string]*budgetTracker),
		agentBudgets: make(map[string][]string, len(agentBudgets)),
	}
	if t := newBudgetTracker(runBudget); t != nil {
		s.trackers[runBudgetName] = t
		s.order = append(s.order, runBudgetName)
	}

	referenced := make(map[string]bool)
	for _, names := range agentBudgets {
		for _, n := range names {
			referenced[n] = true
		}
	}
	namedOrder := make([]string, 0, len(referenced))
	for n := range referenced {
		cfg, ok := named[n]
		if !ok {
			continue
		}
		if t := newBudgetTracker(&cfg); t != nil {
			s.trackers[n] = t
			namedOrder = append(namedOrder, n)
		}
	}
	sort.Strings(namedOrder)
	s.order = append(s.order, namedOrder...)

	for agentName, names := range agentBudgets {
		list := make([]string, 0, len(names)+1)
		if _, ok := s.trackers[runBudgetName]; ok {
			list = append(list, runBudgetName)
		}
		for _, n := range names {
			if _, ok := s.trackers[n]; ok {
				list = append(list, n)
			}
		}
		s.agentBudgets[agentName] = list
	}

	if len(s.trackers) == 0 {
		return nil
	}
	return s
}

func (r *LocalRuntime) budgetRoot(sess *session.Session) string {
	if r.subagents != nil {
		return r.subagents.rootSessionLockedSafe(sess.ID)
	}
	if sess.ParentID != "" {
		return sess.ParentID
	}
	return sess.ID
}

func (r *LocalRuntime) rootBudget(sess *session.Session) *budgetSet {
	if sess == nil || (r.budgetCfg.IsZero() && len(r.budgetsCfg) == 0) {
		return nil
	}
	root := r.budgetRoot(sess)
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	if r.rootBudgets == nil {
		r.rootBudgets = make(map[string]*budgetSet)
	}
	b, exists := r.rootBudgets[root]
	if !exists {
		b = newBudgetSet(r.budgetCfg, r.budgetsCfg, r.agentBudgets)
		r.rootBudgets[root] = b
	}
	return b
}

// budgetContext bounds the whole execution, including provider and tool waits.
func (r *LocalRuntime) budgetContext(ctx context.Context, sess *session.Session, a *agent.Agent) (context.Context, context.CancelFunc) {
	wallet := r.rootBudget(sess)
	if a == nil || wallet == nil {
		return context.WithCancel(ctx)
	}
	now := time.Now()
	var deadline time.Time
	for _, nt := range wallet.budgetsFor(a.Name()) {
		b := nt.Tracker
		b.mu.Lock()
		if b.started.IsZero() {
			b.started = now
		}
		if b.maxTime > 0 {
			end := b.started.Add(b.maxTime)
			if deadline.IsZero() || end.Before(deadline) {
				deadline = end
			}
		}
		b.mu.Unlock()
	}
	var bounded context.Context
	var cancel context.CancelFunc
	if deadline.IsZero() {
		bounded, cancel = context.WithCancel(ctx)
	} else {
		bounded, cancel = context.WithDeadline(ctx, deadline)
	}
	wallet.contextMu.Lock()
	if wallet.contexts == nil {
		wallet.contexts = make(map[*context.CancelFunc]string)
	}
	wallet.contexts[&cancel] = a.Name()
	wallet.contextMu.Unlock()
	context.AfterFunc(bounded, func() {
		wallet.contextMu.Lock()
		defer wallet.contextMu.Unlock()
		delete(wallet.contexts, &cancel)
	})
	wallet.cancelExceeded()
	return bounded, cancel
}

func (s *budgetSet) cancelExceeded() {
	s.contextMu.Lock()
	var cancels []context.CancelFunc
	for cancel, agentName := range s.contexts {
		if s.exceededFor(agentName) != nil {
			cancels = append(cancels, *cancel)
		}
	}
	s.contextMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (r *LocalRuntime) enforceBudget(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	events EventSink,
) iterationDecision {
	breach := r.rootBudget(sess).exceededFor(a.Name())
	if breach == nil {
		return iterationContinue
	}

	slog.InfoContext(ctx, "Run budget exceeded",
		"agent", a.Name(),
		"session_id", sess.ID,
		"budget", breach.Budget,
		"limit", string(breach.Limit),
		"used", breach.Used,
		"max", breach.Max,
	)

	// Build the stop message first so the budget_exceeded event and the
	// session share one canonical message: message_added is not serialized,
	// so the event's stop_message is what JSON consumers rebuild it from.
	stopMsg := &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   breach.Message(),
		CreatedAt: r.now().Format(time.RFC3339),
	}

	events.Emit(BudgetExceeded(sess.ID, a.Name(), *breach, stopMsg))
	r.notifyBudgetExceeded(ctx, a, sess.ID, breach.Message())

	addAgentMessage(sess, a, stopMsg, events)

	return iterationStop
}

func (s *budgetSet) exceededFor(agentName string) *budgetBreach {
	for _, nt := range s.budgetsFor(agentName) {
		if br := nt.Tracker.exceeded(); br != nil {
			br.Budget = nt.Name
			return br
		}
	}
	return nil
}

func (r *LocalRuntime) recordBudget(sess *session.Session, a *agent.Agent, usage *chat.Usage, cost *float64, active time.Duration, events EventSink) {
	s := r.rootBudget(sess)
	if s == nil {
		return
	}
	targets := s.budgetsFor(a.Name())
	if len(targets) == 0 {
		return
	}

	warnUnpriced := !s.unpricedSpend()
	for _, nt := range targets {
		nt.Tracker.record(a.Name(), usage, cost, active)
	}
	if warnUnpriced && s.unpricedSpend() {
		events.Emit(Warning(
			"This run has a max_cost limit, but the model reported usage the runtime cannot price, "+
				"so that spend does not count against the limit. Set a model-level `cost:` block to price it.",
			a.Name(),
		))
	}
	events.Emit(BudgetUsage(sess.ID, a.Name(), s.snapshot()))
	s.cancelExceeded()
}

func (s *budgetSet) unpricedSpend() bool {
	for _, nt := range s.all() {
		if nt.Tracker.unpricedSpend() {
			return true
		}
	}
	return false
}

func (s *budgetSet) snapshot() []namedBudgetSnapshot {
	all := s.all()
	if len(all) == 0 {
		return nil
	}
	out := make([]namedBudgetSnapshot, 0, len(all))
	for _, nt := range all {
		out = append(out, namedBudgetSnapshot{
			Name:     nt.Name,
			Snapshot: nt.Tracker.snapshot(),
		})
	}
	return out
}

type namedBudgetSnapshot struct {
	Name     string
	Snapshot budgetSnapshot
}
