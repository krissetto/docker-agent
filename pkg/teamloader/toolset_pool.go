package teamloader

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
)

const toolsetPoolCleanupTimeout = 5 * time.Second

var ErrToolsetPoolLeasesOutstanding = errors.New("toolset pool leases remain outstanding")

// ToolsetLeasePool shares raw owners only inside a caller-provided source,
// runtime, and session-owner scope. Never share a pool across session resource
// owners: StopResourceOwner is session-scoped and may terminate that owner's jobs.
// Internal HMAC keys are memory-only and never logged or persisted.
type ToolsetLeasePool struct {
	mu        sync.Mutex
	scope     string
	key       [32]byte
	entries   map[[32]byte]*poolEntry
	closed    bool
	changed   chan struct{}
	closeErrs []error
}
type poolEntry struct {
	key          [32]byte
	raw          tools.ToolSet
	dispose      func(context.Context, tools.ToolSet) error
	refs         int
	creating     bool
	closing      bool
	ready        chan struct{}
	startMu      sync.Mutex
	started      bool
	startAttempt *startAttempt
}
type startAttempt struct {
	done chan struct{}
	err  error
}

// NewToolsetLeasePool creates an independent source/runtime-scoped pool.
// scope must be a stable opaque identity and must not contain credentials.
func NewToolsetLeasePool(scope string) (*ToolsetLeasePool, error) {
	if scope == "" {
		return nil, errors.New("toolset pool scope is required")
	}
	p := &ToolsetLeasePool{scope: scope, entries: make(map[[32]byte]*poolEntry), changed: make(chan struct{})}
	if _, err := rand.Read(p.key[:]); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *ToolsetLeasePool) signalLocked() { close(p.changed); p.changed = make(chan struct{}) }
func (p *ToolsetLeasePool) keyFor(material []byte) [32]byte {
	mac := hmac.New(sha256.New, p.key[:])
	_, _ = mac.Write([]byte(p.scope))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(material)
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	return key
}

// acquire coalesces identical concurrent creates without running constructors
// while holding the pool mutex. create must receive the exact effective inputs
// encoded in material; dispose owns final raw-resource teardown.
func (p *ToolsetLeasePool) acquire(ctx context.Context, material []byte, create func() (tools.ToolSet, error), dispose func(context.Context, tools.ToolSet) error) (tools.ToolSet, io.Closer, error) {
	if create == nil || dispose == nil {
		return nil, nil, errors.New("toolset pool requires create and dispose functions")
	}
	key := p.keyFor(material)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, errors.New("toolset pool is closed")
		}
		if entry := p.entries[key]; entry != nil {
			if entry.closing {
				changed := p.changed
				p.mu.Unlock()
				select {
				case <-changed:
					continue
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			if entry.creating {
				ready := entry.ready
				p.mu.Unlock()
				select {
				case <-ready:
					continue
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			entry.refs++
			lease := &toolsetPoolLease{ToolSet: entry.raw, pool: p, entry: entry}
			p.mu.Unlock()
			return lease, lease, nil
		}
		entry := &poolEntry{key: key, creating: true, ready: make(chan struct{}), dispose: dispose}
		p.entries[key] = entry
		p.signalLocked()
		p.mu.Unlock()
		if err := ctx.Err(); err != nil {
			p.failCreate(entry)
			return nil, nil, err
		}
		raw, err := create()
		if err != nil {
			p.failCreate(entry)
			return nil, nil, err
		}
		if raw == nil {
			p.failCreate(entry)
			return nil, nil, errors.New("toolset creator returned nil")
		}
		if err := ctx.Err(); err != nil {
			_ = disposeRawToolset(ctx, dispose, raw)
			p.failCreate(entry)
			return nil, nil, err
		}
		p.mu.Lock()
		if p.closed || p.entries[key] != entry {
			entry.creating = false
			close(entry.ready)
			delete(p.entries, key)
			p.signalLocked()
			p.mu.Unlock()
			_ = disposeRawToolset(ctx, dispose, raw)
			return nil, nil, errors.New("toolset pool closed during creation")
		}
		entry.raw, entry.creating, entry.refs = raw, false, 1
		close(entry.ready)
		p.signalLocked()
		lease := &toolsetPoolLease{ToolSet: raw, pool: p, entry: entry}
		p.mu.Unlock()
		return lease, lease, nil
	}
}

func (p *ToolsetLeasePool) failCreate(entry *poolEntry) {
	p.mu.Lock()
	entry.creating = false
	if p.entries[entry.key] == entry {
		delete(p.entries, entry.key)
	}
	close(entry.ready)
	p.signalLocked()
	p.mu.Unlock()
}

type toolsetPoolLease struct {
	tools.ToolSet
	pool  *ToolsetLeasePool
	entry *poolEntry
	once  sync.Once
	err   error
}

func (l *toolsetPoolLease) Unwrap() tools.ToolSet { return l.ToolSet }
func (l *toolsetPoolLease) Start(ctx context.Context) error {
	for {
		e := l.entry
		e.startMu.Lock()
		if e.started {
			e.startMu.Unlock()
			return nil
		}
		if attempt := e.startAttempt; attempt != nil {
			e.startMu.Unlock()
			select {
			case <-attempt.done:
				if attempt.err != nil {
					return attempt.err
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		attempt := &startAttempt{done: make(chan struct{})}
		e.startAttempt = attempt
		e.startMu.Unlock()
		if startable, ok := tools.As[tools.Startable](l.ToolSet); ok {
			attempt.err = startable.Start(ctx)
		}
		e.startMu.Lock()
		if attempt.err == nil {
			e.started = true
		}
		e.startAttempt = nil
		close(attempt.done)
		e.startMu.Unlock()
		return attempt.err
	}
}

// Stop is per-team retirement and deliberately does not stop the shared raw
// owner. Final pool release calls dispose; session resources use StopResourceOwner.
func (*toolsetPoolLease) Stop(context.Context) error { return nil }
func (l *toolsetPoolLease) StopResourceOwner(ctx context.Context) error {
	if stopper, ok := tools.As[tools.ResourceOwnerStopper](l.ToolSet); ok {
		return stopper.StopResourceOwner(ctx)
	}
	return nil
}
func (l *toolsetPoolLease) Instructions() string { return tools.GetInstructions(l.ToolSet) }
func (l *toolsetPoolLease) Close() error {
	l.once.Do(func() { l.err = l.pool.release(l.entry) })
	return l.err
}

func (p *ToolsetLeasePool) release(entry *poolEntry) error {
	p.mu.Lock()
	if p.entries[entry.key] != entry || entry.refs <= 0 {
		p.mu.Unlock()
		return errors.New("toolset pool lease underflow or stale lease")
	}
	entry.refs--
	dispose := entry.refs == 0 && !entry.closing
	if dispose {
		entry.closing = true
	}
	p.signalLocked()
	p.mu.Unlock()
	if dispose {
		return p.dispose(entry)
	}
	return nil
}
func (p *ToolsetLeasePool) dispose(entry *poolEntry) error {
	var err error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("pooled toolset disposer panicked: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), toolsetPoolCleanupTimeout)
		defer cancel()
		err = entry.dispose(ctx, entry.raw)
	}()
	p.mu.Lock()
	if p.entries[entry.key] == entry {
		delete(p.entries, entry.key)
	}
	entry.closing = false
	if err != nil {
		p.closeErrs = append(p.closeErrs, err)
	}
	p.signalLocked()
	p.mu.Unlock()
	return err
}
func disposeRawToolset(ctx context.Context, dispose func(context.Context, tools.ToolSet) error, raw tools.ToolSet) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolsetPoolCleanupTimeout)
	defer cancel()
	return dispose(cleanupCtx, raw)
}

func (p *ToolsetLeasePool) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.signalLocked()
	for {
		if len(p.entries) == 0 {
			errs := append([]error(nil), p.closeErrs...)
			p.mu.Unlock()
			return errors.Join(errs...)
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
			p.mu.Lock()
		case <-ctx.Done():
			return errors.Join(ctx.Err(), ErrToolsetPoolLeasesOutstanding)
		}
	}
}

type toolsetPoolKeyMaterial struct {
	ConfigName   string   `json:"config_name"`
	ParentDir    string   `json:"parent_dir"`
	SourceScope  string   `json:"source_scope"`
	WorkingDir   string   `json:"working_dir"`
	ToolsetJSON  string   `json:"toolset"`
	EffectiveEnv []string `json:"effective_env"`
}

func makeToolsetPoolKey(configName, parentDir, sourceScope, workingDir, toolsetJSON string, effectiveEnv []string) ([]byte, error) {
	return json.Marshal(toolsetPoolKeyMaterial{configName, parentDir, sourceScope, workingDir, toolsetJSON, effectiveEnv})
}
func toolsetJSON(toolset any) (string, error) {
	data, err := json.Marshal(toolset)
	return string(data), err
}
func sortedEnvironment(env []string) []string { return slices.Clone(env) }
