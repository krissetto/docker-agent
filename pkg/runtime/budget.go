package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
	return b.snapshotWithLimits(latest.BudgetConfig{MaxCost: b.maxCost, MaxTokens: b.maxTokens, MaxTime: latest.Duration{Duration: b.maxTime}})
}

func (b *budgetTracker) snapshotWithLimits(limits latest.BudgetConfig) budgetSnapshot {
	if b == nil {
		return budgetSnapshot{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return budgetSnapshot{Cost: b.cost, MaxCost: limits.MaxCost, Tokens: b.tokens, MaxTokens: limits.MaxTokens, Elapsed: b.elapsedLocked(), MaxTime: limits.MaxTime.Duration, Unpriced: b.unpriced, PerAgent: b.perAgentSpendLocked()}
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
	return b.exceededWithLimits(latest.BudgetConfig{MaxCost: b.maxCost, MaxTokens: b.maxTokens, MaxTime: latest.Duration{Duration: b.maxTime}})
}

func (b *budgetTracker) exceededWithLimits(limits latest.BudgetConfig) *budgetBreach {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if limits.MaxCost > 0 && b.cost >= limits.MaxCost {
		return &budgetBreach{
			Limit: budgetLimitCost,
			Used:  formatUSD(b.cost),
			Max:   formatUSD(limits.MaxCost),
		}
	}
	if limits.MaxTokens > 0 && b.tokens >= limits.MaxTokens {
		return &budgetBreach{
			Limit: budgetLimitTokens,
			Used:  fmt.Sprintf("%d tokens", b.tokens),
			Max:   fmt.Sprintf("%d tokens", limits.MaxTokens),
		}
	}
	if limits.MaxTime.Duration > 0 && b.elapsedLocked() >= limits.MaxTime.Duration {
		return &budgetBreach{
			Limit: budgetLimitTime,
			Used:  b.elapsedLocked().Round(time.Second).String(),
			Max:   limits.MaxTime.Duration.String(),
		}
	}
	return nil
}

func (b *budgetTracker) hasUnpricedSpend() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unpriced
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

// unpricedSpendWarning explains why spend the runtime cannot price does not
// count against a max_cost limit.
const unpricedSpendWarning = "This run has a max_cost limit, but the model reported usage the runtime cannot price, " +
	"so that spend does not count against the limit. Set a model-level `cost:` block to price it."

type namedTracker struct {
	Name    string
	Tracker *budgetTracker
}

type budgetSet struct {
	trackers     map[string]*budgetTracker // shared spend accounts; never remove pots during a root's life
	trackerMu    sync.Mutex
	agentBudgets map[string][]string // legacy policy only
	order        []string            // legacy policy only
	contextMu    sync.Mutex
	contexts     map[*context.CancelFunc]budgetContextBinding
	rebindings   map[*executionBudgetBinding]struct{}
}

type budgetContextBinding struct {
	agentName string
	policy    *budgetPolicy
}
type budgetPolicy struct {
	limits       map[string]latest.BudgetConfig
	pots         map[string]bool
	agentBudgets map[string][]string
	order        []string
}
type budgetView struct {
	ledger *budgetSet
	policy *budgetPolicy
	legacy bool
}

func newBudgetPolicy(cfg BudgetPolicyConfig) *budgetPolicy {
	policy := &budgetPolicy{limits: make(map[string]latest.BudgetConfig), pots: make(map[string]bool), agentBudgets: make(map[string][]string)}
	if cfg.Run != nil && !cfg.Run.IsZero() {
		policy.limits[runBudgetName] = *cfg.Run
	}
	for name, limit := range cfg.Named {
		policy.pots[name] = true
		if !limit.IsZero() {
			policy.limits[name] = limit
		}
	}
	for name, refs := range cfg.AgentBudgets {
		policy.agentBudgets[name] = append([]string(nil), refs...)
	}
	seen := map[string]bool{}
	if _, ok := policy.limits[runBudgetName]; ok {
		policy.order = append(policy.order, runBudgetName)
		seen[runBudgetName] = true
	}
	var named []string
	for _, refs := range policy.agentBudgets {
		for _, name := range refs {
			if _, ok := policy.limits[name]; ok && !seen[name] {
				seen[name] = true
				named = append(named, name)
			}
		}
	}
	sort.Strings(named)
	policy.order = append(policy.order, named...)
	return policy
}

func (s *budgetSet) ensureAccount(name string) *budgetTracker {
	s.trackerMu.Lock()
	defer s.trackerMu.Unlock()
	if s.trackers == nil {
		s.trackers = make(map[string]*budgetTracker)
	}
	if account := s.trackers[name]; account != nil {
		return account
	}
	account := newBudgetTracker(nil)
	if account == nil {
		account = &budgetTracker{perAgent: make(map[string]*agentSpend)}
	}
	s.trackers[name] = account
	return account
}

func (p *budgetPolicy) namesFor(agentName string) []string {
	if p == nil {
		return nil
	}
	names := []string{}
	if _, ok := p.limits[runBudgetName]; ok {
		names = append(names, runBudgetName)
	}
	refs := p.agentBudgets[agentName]
	if len(refs) == 0 {
		return names
	}
	for _, name := range refs {
		if p.pots[name] {
			names = append(names, name)
		}
	}
	return names
}

func (v *budgetView) budgetNamesFor(agentName string, chargeRun bool) []string {
	if v == nil || v.ledger == nil {
		return nil
	}
	if v.legacy {
		names := v.ledger.agentBudgets[agentName]
		if len(names) == 0 {
			if v.ledger.trackers[runBudgetName] != nil {
				return []string{runBudgetName}
			}
			return nil
		}
		selected := make([]string, 0, len(names)+1)
		hasRun := slices.Contains(names, runBudgetName)
		if !hasRun && v.ledger.trackers[runBudgetName] != nil {
			selected = append(selected, runBudgetName)
		}
		for _, name := range names {
			if v.ledger.trackers[name] != nil {
				selected = append(selected, name)
			}
		}
		return selected
	}
	names := v.policy.namesFor(agentName)
	if chargeRun && !slices.Contains(names, runBudgetName) {
		names = append([]string{runBudgetName}, names...)
	}
	return names
}

func namesFromTrackers(trackers []namedTracker) []string {
	names := make([]string, 0, len(trackers))
	for _, nt := range trackers {
		names = append(names, nt.Name)
	}
	return names
}

func (v *budgetView) budgetsFor(agentName string, chargeRun bool) []namedTracker {
	names := v.budgetNamesFor(agentName, chargeRun)
	result := make([]namedTracker, 0, len(names))
	for _, name := range names {
		var tracker *budgetTracker
		if v.legacy {
			tracker = v.ledger.trackers[name]
		} else {
			tracker = v.ledger.ensureAccount(name)
		}
		if tracker != nil {
			result = append(result, namedTracker{Name: name, Tracker: tracker})
		}
	}
	return result
}

func (v *budgetView) exceededFor(agentName string) *budgetBreach {
	if v == nil || v.policy == nil {
		return nil
	}
	for _, name := range v.policy.namesFor(agentName) {
		limit := v.policy.limits[name]
		account := v.ledger.ensureAccount(name)
		if breach := account.exceededWithLimits(limit); breach != nil {
			breach.Budget = name
			return breach
		}
	}
	return nil
}

func (v *budgetView) snapshot() []namedBudgetSnapshot {
	if v == nil || v.policy == nil {
		return nil
	}
	out := make([]namedBudgetSnapshot, 0, len(v.policy.order))
	for _, name := range v.policy.order {
		limit := v.policy.limits[name]
		account := v.ledger.ensureAccount(name)
		out = append(out, namedBudgetSnapshot{Name: name, Snapshot: account.snapshotWithLimits(limit)})
	}
	return out
}

func (s *budgetSet) budgetsFor(agentName string) []namedTracker {
	return (&budgetView{ledger: s, legacy: true}).budgetsFor(agentName, false)
}
func (s *budgetSet) all() []namedTracker {
	if s == nil {
		return nil
	}
	names := append([]string(nil), s.order...)
	out := make([]namedTracker, 0, len(names))
	for _, name := range names {
		if tracker := s.trackers[name]; tracker != nil {
			out = append(out, namedTracker{Name: name, Tracker: tracker})
		}
	}
	return out
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
func (s *budgetSet) snapshot() []namedBudgetSnapshot {
	all := s.all()
	out := make([]namedBudgetSnapshot, 0, len(all))
	for _, nt := range all {
		out = append(out, namedBudgetSnapshot{Name: nt.Name, Snapshot: nt.Tracker.snapshot()})
	}
	return out
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
			if n == runBudgetName {
				continue
			}
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
	if sess == nil {
		return nil
	}
	root := r.budgetRoot(sess)
	r.budgetMu.Lock()
	defer r.budgetMu.Unlock()
	if b, exists := r.rootBudgets[root]; exists {
		return b
	}
	if r.budgetCfg.IsZero() && len(r.budgetsCfg) == 0 {
		return nil
	}
	if r.rootBudgets == nil {
		r.rootBudgets = make(map[string]*budgetSet)
	}
	b := newBudgetSet(r.budgetCfg, r.budgetsCfg, r.agentBudgets)
	r.rootBudgets[root] = b
	return b
}

func (r *LocalRuntime) budgetViewForContext(ctx context.Context, sess *session.Session) *budgetView {
	if sess == nil {
		return nil
	}
	if env := teamEnvironmentFromContext(ctx); env != nil {
		root := r.budgetRoot(sess)
		r.budgetMu.Lock()
		if r.rootBudgets == nil {
			r.rootBudgets = make(map[string]*budgetSet)
		}
		ledger := r.rootBudgets[root]
		if ledger == nil {
			ledger = &budgetSet{trackers: make(map[string]*budgetTracker)}
			r.rootBudgets[root] = ledger
		}
		r.budgetMu.Unlock()
		policy := newBudgetPolicy(env.BudgetPolicy())
		return &budgetView{ledger: ledger, policy: policy}
	}
	if r.teamEnvironments != nil {
		return nil
	}
	ledger := r.rootBudget(sess)
	if ledger == nil {
		return nil
	}
	return &budgetView{ledger: ledger, legacy: true}
}

func (r *LocalRuntime) budgetUsageForContext(ctx context.Context, sess *session.Session, agentName string) []namedBudgetSnapshot {
	view := r.budgetViewForContext(ctx, sess)
	if view == nil {
		return nil
	}
	if view.legacy {
		return view.ledger.snapshot()
	}
	return view.snapshot()
}

// budgetBreachFor reports the first budget the agent has exhausted on the
// root ledger that governs sess, or nil. It records nothing.
func (r *LocalRuntime) budgetBreachFor(ctx context.Context, sess *session.Session, a *agent.Agent) *budgetBreach {
	view := r.budgetViewForContext(ctx, sess)
	if view == nil {
		return nil
	}
	if view.legacy {
		return view.ledger.exceededFor(a.Name())
	}
	return view.exceededFor(a.Name())
}

func (r *LocalRuntime) enforceBudgetForContext(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) iterationDecision {
	breach := r.budgetBreachFor(ctx, sess, a)
	if breach == nil {
		return iterationContinue
	}
	slog.InfoContext(ctx, "Run budget exceeded", "agent", a.Name(), "session_id", sess.ID, "budget", breach.Budget, "limit", string(breach.Limit), "used", breach.Used, "max", breach.Max)
	stopMsg := &chat.Message{Role: chat.MessageRoleAssistant, Content: breach.Message(), CreatedAt: r.now().Format(time.RFC3339)}
	events.Emit(BudgetExceeded(sess.ID, a.Name(), *breach, stopMsg))
	r.notifyBudgetExceeded(ctx, a, sess.ID, breach.Message())
	addAgentMessage(sess, a, stopMsg, events)
	return iterationStop
}

func (r *LocalRuntime) recordBudgetForContext(ctx context.Context, sess *session.Session, a *agent.Agent, usage *chat.Usage, cost *float64, active time.Duration, events EventSink) {
	if r.compactionSpend != nil && usage != nil {
		// A compaction summary runs on a child runtime; its spend is charged
		// to the parent's budgets once the compaction settles.
		r.compactionSpend.record(compactionCharge{usage: *usage, cost: cost})
		return
	}
	view := r.budgetViewForContext(ctx, sess)
	if view == nil {
		return
	}
	chargeRun := !view.legacy
	targets := view.budgetsFor(a.Name(), chargeRun)
	if len(targets) == 0 {
		return
	}
	warnUnpriced := false
	for _, nt := range targets {
		if nt.Tracker.hasUnpricedSpend() {
			warnUnpriced = true
		}
		nt.Tracker.record(a.Name(), usage, cost, active)
		if nt.Tracker.hasUnpricedSpend() {
			warnUnpriced = true
		}
	}
	if warnUnpriced {
		hasCostLimit := false
		if view.legacy {
			hasCostLimit = true
		} else {
			for _, name := range view.policy.namesFor(a.Name()) {
				if view.policy.limits[name].MaxCost > 0 {
					hasCostLimit = true
				}
			}
		}
		if hasCostLimit {
			events.Emit(Warning(unpricedSpendWarning, a.Name()))
		}
	}
	if view.legacy {
		events.Emit(BudgetUsage(sess.ID, a.Name(), view.ledger.snapshot()))
		view.ledger.cancelExceeded()
	} else {
		events.Emit(BudgetUsage(sess.ID, a.Name(), view.snapshot()))
		view.ledger.cancelExceededPolicy(view.policy)
	}
}

type executionBudgetKey struct{}

type executionBudgetBinding struct {
	wallet    *budgetSet
	policy    *budgetPolicy
	legacy    bool
	agentName string
	cancel    context.CancelFunc
	timer     *time.Timer
	revision  uint64
}

// The generation context stays stable while named-wallet membership changes.
func (r *LocalRuntime) executionBudgetContext(ctx context.Context, sess *session.Session, a *agent.Agent) (context.Context, context.CancelFunc) {
	view := r.budgetViewForContext(ctx, sess)
	if view == nil || a == nil {
		return context.WithCancel(ctx)
	}
	wallet := view.ledger
	bounded, cancel := context.WithCancel(ctx)
	binding := &executionBudgetBinding{wallet: wallet, policy: view.policy, legacy: view.legacy, cancel: cancel}
	wallet.contextMu.Lock()
	if wallet.rebindings == nil {
		wallet.rebindings = make(map[*executionBudgetBinding]struct{})
	}
	wallet.rebindings[binding] = struct{}{}
	binding.rebindLocked(a.Name())
	wallet.contextMu.Unlock()
	cleanup := func() {
		wallet.contextMu.Lock()
		delete(wallet.rebindings, binding)
		binding.revision++
		if binding.timer != nil {
			binding.timer.Stop()
		}
		wallet.contextMu.Unlock()
		cancel()
	}
	context.AfterFunc(bounded, cleanup)
	wallet.cancelExceeded()
	if !view.legacy {
		wallet.cancelExceededPolicy(view.policy)
	}
	return context.WithValue(bounded, executionBudgetKey{}, binding), cleanup
}

func (b *executionBudgetBinding) rebindLocked(name string) {
	b.agentName = name
	b.revision++
	if b.timer != nil {
		b.timer.Stop()
	}
	var deadline time.Time
	now := time.Now()
	view := &budgetView{ledger: b.wallet, policy: b.policy, legacy: b.legacy}
	for _, nt := range view.budgetsFor(name, !b.legacy) {
		t := nt.Tracker
		t.mu.Lock()
		if t.started.IsZero() {
			t.started = now
		}
		maxTime := t.maxTime
		if !b.legacy {
			maxTime = b.policy.limits[nt.Name].MaxTime.Duration
		}
		if maxTime > 0 {
			end := t.started.Add(maxTime)
			if deadline.IsZero() || end.Before(deadline) {
				deadline = end
			}
		}
		t.mu.Unlock()
	}
	if !deadline.IsZero() {
		revision := b.revision
		b.timer = time.AfterFunc(time.Until(deadline), func() {
			b.wallet.contextMu.Lock()
			defer b.wallet.contextMu.Unlock()
			if b.revision == revision {
				b.cancel()
			}
		})
	}
	if b.legacy {
		if b.wallet.exceededFor(name) != nil {
			b.cancel()
		}
	} else if view.exceededFor(name) != nil {
		b.cancel()
	}
}

func rebindExecutionBudget(ctx context.Context, name string) {
	if binding, ok := ctx.Value(executionBudgetKey{}).(*executionBudgetBinding); ok {
		binding.wallet.contextMu.Lock()
		if _, registered := binding.wallet.rebindings[binding]; registered {
			binding.rebindLocked(name)
		}
		binding.wallet.contextMu.Unlock()
	}
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
		wallet.contexts = make(map[*context.CancelFunc]budgetContextBinding)
	}
	wallet.contexts[&cancel] = budgetContextBinding{agentName: a.Name()}
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
	for cancel, binding := range s.contexts {
		view := &budgetView{ledger: s, policy: binding.policy, legacy: binding.policy == nil}
		var breach *budgetBreach
		if view.legacy {
			breach = s.exceededFor(binding.agentName)
		} else {
			breach = view.exceededFor(binding.agentName)
		}
		if breach != nil {
			cancels = append(cancels, *cancel)
		}
	}
	for binding := range s.rebindings {
		view := &budgetView{ledger: s, policy: binding.policy, legacy: binding.legacy}
		var breach *budgetBreach
		if binding.legacy {
			breach = s.exceededFor(binding.agentName)
		} else {
			breach = view.exceededFor(binding.agentName)
		}
		if breach != nil {
			binding.cancel()
		}
	}
	s.contextMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *budgetSet) cancelExceededPolicy(_ *budgetPolicy) {
	s.contextMu.Lock()
	var cancels []context.CancelFunc
	for cancel, binding := range s.contexts {
		view := &budgetView{ledger: s, policy: binding.policy}
		if view.exceededFor(binding.agentName) != nil {
			cancels = append(cancels, *cancel)
		}
	}
	for binding := range s.rebindings {
		if (&budgetView{ledger: s, policy: binding.policy, legacy: binding.legacy}).exceededFor(binding.agentName) != nil {
			binding.cancel()
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
		events.Emit(Warning(unpricedSpendWarning, a.Name()))
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

type namedBudgetSnapshot struct {
	Name     string
	Snapshot budgetSnapshot
}
