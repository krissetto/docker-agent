package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/team"
)

const teamEnvironmentCleanupTimeout = 5 * time.Second

var ErrTeamEnvironmentLeasesOutstanding = errors.New("team environment leases remain outstanding")

// TeamEnvironment is a fully built, immutable team revision. The Team pointer
// is build-owned and callers must not mutate it after publication. dispose owns
// all resources created for the build; shared-resource disposers must implement
// their own refcounting when builders reuse resource owners.
// BudgetPolicyConfig is an immutable revision's budget policy. Usage is
// accounted separately by the runtime's per-root ledger so edits can change
// limits without resetting already-spent cost, tokens, or time.
type BudgetPolicyConfig struct {
	Run          *latest.BudgetConfig
	Named        map[string]latest.BudgetConfig
	AgentBudgets map[string][]string
}

type TeamEnvironmentOpt func(*TeamEnvironment)

func WithTeamEnvironmentBudgetConfig(config BudgetPolicyConfig) TeamEnvironmentOpt {
	return func(env *TeamEnvironment) { env.budget = cloneBudgetPolicyConfig(config) }
}

func cloneBudgetPolicyConfig(input BudgetPolicyConfig) BudgetPolicyConfig {
	output := BudgetPolicyConfig{Named: make(map[string]latest.BudgetConfig, len(input.Named)), AgentBudgets: make(map[string][]string, len(input.AgentBudgets))}
	if input.Run != nil {
		run := *input.Run
		output.Run = &run
	}
	for name, budget := range input.Named {
		output.Named[name] = budget
	}
	for name, budgets := range input.AgentBudgets {
		output.AgentBudgets[name] = slices.Clone(budgets)
	}
	return output
}

func (e *TeamEnvironment) BudgetPolicy() BudgetPolicyConfig { return cloneBudgetPolicyConfig(e.budget) }

type TeamEnvironment struct {
	revisionID string
	sourceID   string
	baselineID string
	teamID     string
	team       *team.Team
	models     ModelSwitcherConfig
	budget     BudgetPolicyConfig
	dispose    func(context.Context) error

	ownerMu    sync.Mutex            // serializes transfer into exactly one store or discard
	owner      *TeamEnvironmentStore // set on publication; nil means builder still owns it
	discarding bool                  // ownerMu: disposal is in flight for unpublished env
	discarded  bool                  // ownerMu: unpublished env disposal completed
	refs       int                   // store.mu
	retired    bool                  // store.mu
	disposing  bool                  // store.mu
	disposed   bool                  // store.mu
	disposeErr error                 // store.mu
	done       chan struct{}
}

// NewTeamEnvironment creates a build-owned environment. On successful Publish
// ownership transfers to the store. On failed Publish the builder still owns
// the candidate and must invoke its own disposal path.
func NewTeamEnvironment(revisionID, sourceID, baselineID, teamID string, tm *team.Team, models ModelSwitcherConfig, dispose func(context.Context) error, opts ...TeamEnvironmentOpt) (*TeamEnvironment, error) {
	if revisionID == "" || sourceID == "" || baselineID == "" || teamID == "" || tm == nil || dispose == nil {
		return nil, errors.New("team environment requires revision/source/baseline/team identity, team, and disposer")
	}
	modelCopy, err := cloneEnvironmentModelConfig(models)
	if err != nil {
		return nil, fmt.Errorf("copying team model configuration: %w", err)
	}
	env := &TeamEnvironment{
		revisionID: revisionID,
		sourceID:   sourceID,
		baselineID: baselineID,
		teamID:     teamID,
		team:       tm,
		models:     modelCopy,
		dispose:    dispose,
		done:       make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(env)
		}
	}
	return env, nil
}

func cloneEnvironmentModelConfig(input ModelSwitcherConfig) (ModelSwitcherConfig, error) {
	output := input
	output.Models, output.Providers = nil, nil
	if input.Models != nil {
		output.Models = make(map[string]latest.ModelConfig, len(input.Models))
		for key, model := range input.Models {
			var clone latest.ModelConfig
			types.CloneThroughJSON(model, &clone)
			clone.Name = model.Name
			clone.DisplayModel = model.DisplayModel
			output.Models[key] = clone
		}
	}
	if input.Providers != nil {
		output.Providers = make(map[string]latest.ProviderConfig, len(input.Providers))
		for key, provider := range input.Providers {
			var clone latest.ProviderConfig
			types.CloneThroughJSON(provider, &clone)
			output.Providers[key] = clone
		}
	}
	output.AgentDefaultModels = maps.Clone(input.AgentDefaultModels)
	return output, nil
}

func (e *TeamEnvironment) RevisionID() string { return e.revisionID }
func (e *TeamEnvironment) SourceID() string   { return e.sourceID }
func (e *TeamEnvironment) BaselineID() string { return e.baselineID }
func (e *TeamEnvironment) TeamID() string     { return e.teamID }

// Discard disposes an unpublished candidate still owned by its builder. It is
// idempotent, runs disposal under an independent bounded context, and rejects
// environments already transferred into a store. Publish and Discard serialize
// on ownerMu so exactly one side can take disposal/publication ownership.
func (e *TeamEnvironment) Discard(context.Context) error {
	e.ownerMu.Lock()
	if e.owner != nil {
		e.ownerMu.Unlock()
		return errors.New("published team environment is owned by its store")
	}
	if e.discarded {
		err := e.disposeErr
		e.ownerMu.Unlock()
		return err
	}
	if e.discarding {
		done := e.done
		e.ownerMu.Unlock()
		<-done
		e.ownerMu.Lock()
		err := e.disposeErr
		e.ownerMu.Unlock()
		return err
	}
	e.discarding = true
	e.ownerMu.Unlock()
	err := disposeCandidate(e)
	e.ownerMu.Lock()
	e.disposeErr = err
	e.discarded = true
	e.discarding = false
	close(e.done)
	e.ownerMu.Unlock()
	return err
}

func disposeCandidate(e *TeamEnvironment) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("disposing team environment panicked: %v", recovered)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), teamEnvironmentCleanupTimeout)
	defer cancel()
	return e.dispose(ctx)
}

// Team returns the build-owned team. Callers must treat it as immutable after
// publication; runtime session operations obtain it through a revision lease.
func (e *TeamEnvironment) Team() *team.Team { return e.team }

// ModelSwitcherConfig returns an independent copy of mutable model maps.
func (e *TeamEnvironment) ModelSwitcherConfig() (ModelSwitcherConfig, error) {
	return cloneEnvironmentModelConfig(e.models)
}

type TeamEnvironmentStore struct {
	mu          sync.Mutex
	current     *TeamEnvironment
	entries     map[string]*TeamEnvironment // current and leased retired revisions
	issued      map[string]struct{}         // IDs never reusable, even after pruning
	disposeErrs []error
	closed      bool
}

func NewTeamEnvironmentStore(initial *TeamEnvironment) (*TeamEnvironmentStore, error) {
	if initial == nil {
		return nil, errors.New("initial team environment is required")
	}
	initial.ownerMu.Lock()
	defer initial.ownerMu.Unlock()
	if initial.owner != nil || initial.discarding || initial.discarded {
		return nil, errors.New("initial team environment already belongs to a store or was discarded")
	}
	s := &TeamEnvironmentStore{current: initial, entries: map[string]*TeamEnvironment{initial.revisionID: initial}, issued: map[string]struct{}{initial.revisionID: {}}}
	initial.owner = s
	return s, nil
}

type TeamEnvironmentLease struct {
	store *TeamEnvironmentStore
	env   *TeamEnvironment
	once  sync.Once
	err   error
}

func (s *TeamEnvironmentStore) AcquireCurrent() (*TeamEnvironmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("team environment store is closed")
	}
	return s.acquireLocked(s.current)
}

// Acquire leases the current revision or a retired revision that is still
// retained by an existing operation. Disposed revisions cannot be resurrected.
func (s *TeamEnvironmentStore) Acquire(revisionID string) (*TeamEnvironmentLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("team environment store is closed")
	}
	env, ok := s.entries[revisionID]
	if !ok || env.disposing || env.disposed {
		return nil, errors.New("team environment revision is no longer retained")
	}
	return s.acquireLocked(env)
}

func (s *TeamEnvironmentStore) acquireLocked(env *TeamEnvironment) (*TeamEnvironmentLease, error) {
	if env == nil || env.owner != s || env.disposing || env.disposed {
		return nil, errors.New("team environment revision is unavailable")
	}
	env.refs++
	return &TeamEnvironmentLease{store: s, env: env}, nil
}

func (l *TeamEnvironmentLease) Environment() *TeamEnvironment {
	if l == nil {
		return nil
	}
	return l.env
}

// Release is idempotent. A caller cancellation cannot skip final disposal;
// cleanup uses its own bounded context. A canceled context may still be
// returned after the lease has been safely released.
func (l *TeamEnvironmentLease) Release(ctx context.Context) error {
	if l == nil || l.env == nil {
		return nil
	}
	l.once.Do(func() { l.err = l.store.release(l.env) })
	if l.err != nil {
		return l.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.store.mu.Lock()
	waiting, done := l.env.disposing || l.env.disposed || (l.env.retired && l.env.refs == 0), l.env.done
	l.store.mu.Unlock()
	if !waiting {
		return nil
	}
	select {
	case <-done:
		l.store.mu.Lock()
		err := l.env.disposeErr
		l.store.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *TeamEnvironmentStore) release(env *TeamEnvironment) error {
	s.mu.Lock()
	if env.owner != s || env.refs <= 0 {
		s.mu.Unlock()
		return errors.New("team environment lease count underflow or foreign lease")
	}
	env.refs--
	start := env.retired && env.refs == 0 && !env.disposed && !env.disposing
	if start {
		env.disposing = true
	}
	s.mu.Unlock()
	if start {
		go s.dispose(env)
	}
	return nil
}

// Publish transfers next to the store only on success. On failure next remains
// caller-owned and must be disposed by its builder. Source/baseline must match
// the current head, and a revision ID cannot be reused.
func (s *TeamEnvironmentStore) Publish(expectedSourceID, expectedRevisionID string, next *TeamEnvironment) error {
	return s.PublishWithCommit(expectedSourceID, expectedRevisionID, next, nil)
}

// PublishWithCommit couples an editor's in-memory source-head CAS with runtime
// environment CAS. commitSource may run only once and must be a bounded,
// in-memory commit callback that returns before mutating on error. No build,
// cleanup, filesystem I/O, or arbitrary user code belongs in this callback.
// After it returns nil, the remaining store swap cannot fail.
func (s *TeamEnvironmentStore) PublishWithCommit(expectedSourceID, expectedRevisionID string, next *TeamEnvironment, commitSource func() error) error {
	if next == nil {
		return errors.New("next team environment is required")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("team environment store is closed")
	}
	current := s.current
	if current == nil || current.sourceID != expectedSourceID || current.revisionID != expectedRevisionID {
		s.mu.Unlock()
		return errors.New("team environment publish conflict")
	}
	if next == current {
		s.mu.Unlock()
		return errors.New("cannot republish the current team environment")
	}
	if next.sourceID != current.sourceID || next.baselineID != current.baselineID {
		s.mu.Unlock()
		return errors.New("replacement team environment crossed source or baseline identity")
	}
	if _, used := s.issued[next.revisionID]; used {
		s.mu.Unlock()
		return errors.New("team environment revision ID was already used")
	}
	next.ownerMu.Lock()
	if next.owner != nil || next.discarding || next.discarded {
		next.ownerMu.Unlock()
		s.mu.Unlock()
		return errors.New("next team environment already belongs to a store or was discarded")
	}
	if commitSource != nil {
		called := false
		atomicCommit := func() error {
			if called {
				return errors.New("source commit callback may run only once")
			}
			if err := commitSource(); err != nil {
				return err
			}
			called = true
			return nil
		}
		if err := atomicCommit(); err != nil {
			next.ownerMu.Unlock()
			s.mu.Unlock()
			return fmt.Errorf("committing authored source head: %w", err)
		}
		if !called {
			next.ownerMu.Unlock()
			s.mu.Unlock()
			return errors.New("source commit callback did not complete")
		}
	}
	next.owner = s
	next.ownerMu.Unlock()
	current.retired = true
	startOld := current.refs == 0 && !current.disposed && !current.disposing
	if startOld {
		current.disposing = true
	}
	s.current = next
	s.entries[next.revisionID] = next
	s.issued[next.revisionID] = struct{}{}
	s.mu.Unlock()
	if startOld {
		go s.dispose(current)
	}
	return nil
}

// Close retires every revision and disposes unleased environments. It never
// waits indefinitely for leaked operation leases; final release still disposes.
func (s *TeamEnvironmentStore) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.current = nil
		for _, env := range s.entries {
			env.retired = true
			if env.refs == 0 && !env.disposing && !env.disposed {
				env.disposing = true
				go s.dispose(env)
			}
		}
	}
	envs := make([]*TeamEnvironment, 0, len(s.entries))
	for _, env := range s.entries {
		envs = append(envs, env)
	}
	s.mu.Unlock()
	for _, env := range envs {
		select {
		case <-env.done:
		case <-ctx.Done():
			s.mu.Lock()
			remaining := 0
			for _, candidate := range envs {
				if candidate.refs > 0 && !candidate.disposed {
					remaining++
				}
			}
			s.mu.Unlock()
			if remaining > 0 {
				return errors.Join(ctx.Err(), ErrTeamEnvironmentLeasesOutstanding)
			}
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.disposeErrs...)
}

func (s *TeamEnvironmentStore) dispose(env *TeamEnvironment) error {
	var disposeErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				disposeErr = fmt.Errorf("disposing team environment panicked: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), teamEnvironmentCleanupTimeout)
		defer cancel()
		disposeErr = env.dispose(ctx)
	}()
	s.mu.Lock()
	env.disposeErr, env.disposed, env.disposing = disposeErr, true, false
	close(env.done)
	if s.entries[env.revisionID] == env {
		delete(s.entries, env.revisionID)
	}
	if disposeErr != nil {
		s.disposeErrs = append(s.disposeErrs, disposeErr)
	}
	s.mu.Unlock()
	return disposeErr
}

// Revisions returns current and not-yet-disposed retired revision IDs.
// SnapshotEnvironments returns borrowed immutable environments currently retained
// by the store. Use only while a live runtime owns the store; callers do not
// acquire or extend leases through this method.
func (s *TeamEnvironmentStore) SnapshotEnvironments() []*TeamEnvironment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*TeamEnvironment, 0, len(s.entries))
	for _, env := range s.entries {
		if !env.disposing && !env.disposed {
			out = append(out, env)
		}
	}
	return out
}

func (s *TeamEnvironmentStore) Revisions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]string, 0, len(s.entries))
	for revision := range s.entries {
		result = append(result, revision)
	}
	slices.Sort(result)
	return result
}
