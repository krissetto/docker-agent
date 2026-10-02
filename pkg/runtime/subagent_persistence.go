package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

const (
	defaultSubagentPersistenceTimeout = 2 * time.Second
	defaultSubagentFlushTimeout       = 3 * time.Second
	subagentPersistenceMaxAttempts    = 4
	subagentPersistenceRetryBase      = 10 * time.Millisecond
)

type subagentPersistence struct {
	treeStore subagent.Store
	ctx       context.Context //nolint:containedctx // lifecycle context owned and canceled by close
	cancel    context.CancelFunc
	wake      chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	closed    bool // admission is closed; failed pending writes remain retryable
	drained   bool
	flushMu   chan struct{}
	trees     map[string]subagent.Snapshot
}

func newSubagentPersistence(treeStore subagent.Store, sessionStore session.Store) *subagentPersistence {
	if treeStore == nil && sessionStore == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &subagentPersistence{
		treeStore: treeStore, ctx: ctx, cancel: cancel, flushMu: make(chan struct{}, 1),
		wake: make(chan struct{}, 1), done: make(chan struct{}), trees: map[string]subagent.Snapshot{},
	}
	go p.run()
	return p
}

func (p *subagentPersistence) enqueueTree(id string, snapshot subagent.Snapshot) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	if !p.closed {
		p.trees[id] = snapshot
	}
	p.mu.Unlock()
	p.signal()
}

func (p *subagentPersistence) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *subagentPersistence) run() {
	defer close(p.done)
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
			if err := p.flushBatch(p.ctx); err != nil {
				slog.Warn("Failed to persist subagent state", "error", err)
			}
		}
	}
}

func (p *subagentPersistence) takeBatch() map[string]subagent.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	trees := p.trees
	p.trees = map[string]subagent.Snapshot{}
	return trees
}

func (p *subagentPersistence) retryTemporary(ctx context.Context, write func(context.Context) error) error {
	var err error
	for attempt := range subagentPersistenceMaxAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
		err = write(writeCtx)
		cancel()
		if err == nil || !session.IsTemporary(err) || attempt+1 == subagentPersistenceMaxAttempts {
			return err
		}
		base := subagentPersistenceRetryBase << attempt
		delay := base/2 + time.Duration(rand.Int64N(int64(base)))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return err
}

func (p *subagentPersistence) requeue(trees map[string]subagent.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, snapshot := range trees {
		if _, newer := p.trees[id]; !newer {
			p.trees[id] = snapshot
		}
	}
}

func (p *subagentPersistence) flushBatch(ctx context.Context) error {
	select {
	case p.flushMu <- struct{}{}:
		defer func() { <-p.flushMu }()
	case <-ctx.Done():
		return ctx.Err()
	}
	trees := p.takeBatch()
	failedTrees := map[string]subagent.Snapshot{}
	var failures []error
	for id, snapshot := range trees {
		if p.treeStore == nil {
			continue
		}
		err := p.retryTemporary(ctx, func(ctx context.Context) error { return p.treeStore.SaveTree(ctx, id, snapshot) })
		if err != nil {
			failedTrees[id] = snapshot
			failures = append(failures, fmt.Errorf("persist subagent tree for session %s: %w", id, err))
		}
	}
	p.requeue(failedTrees)
	return errors.Join(failures...)
}

// flushNow is an explicit durability barrier for every value enqueued before
// the call. Concurrent newer values may supersede a failed covered write.
func (p *subagentPersistence) flushNow() error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultSubagentFlushTimeout)
	defer cancel()
	return p.flushBatch(ctx)
}

func (p *subagentPersistence) close() error {
	return p.closeContext(context.Background())
}

func (p *subagentPersistence) closeContext(ctx context.Context) error {
	if p == nil {
		return nil
	}
	// One shared budget bounds waiting, every root and every retry. The
	// default also bounds callers without their own shutdown deadline.
	ctx, cancel := context.WithTimeout(ctx, defaultSubagentFlushTimeout)
	defer cancel()
	p.mu.Lock()
	if p.drained {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	select {
	case <-p.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := p.flushBatch(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.drained = len(p.trees) == 0
	p.mu.Unlock()
	return nil
}
