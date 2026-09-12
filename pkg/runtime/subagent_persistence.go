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
	treeStore    subagent.Store
	sessionStore session.Store
	ctx          context.Context //nolint:containedctx // lifecycle context owned and canceled by close
	cancel       context.CancelFunc
	wake         chan struct{}
	done         chan struct{}
	mu           sync.Mutex
	closed       bool
	flushMu      sync.Mutex
	trees        map[string]subagent.Snapshot
	transcripts  map[string]transcriptWrite
}

type transcriptWrite struct {
	parentID string
	session  *session.Session
}

func newSubagentPersistence(treeStore subagent.Store, sessionStore session.Store) *subagentPersistence {
	if treeStore == nil && sessionStore == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &subagentPersistence{
		treeStore: treeStore, sessionStore: sessionStore, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}), trees: map[string]subagent.Snapshot{}, transcripts: map[string]transcriptWrite{},
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

func (p *subagentPersistence) enqueueTranscript(parentID string, child *session.Session) {
	if p == nil || parentID == "" || child == nil {
		return
	}
	p.mu.Lock()
	if !p.closed {
		p.transcripts[child.ID] = transcriptWrite{parentID, child.Clone()}
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
			if err := p.flushBatch(); err != nil {
				slog.Warn("Failed to persist subagent state", "error", err)
			}
		}
	}
}

func (p *subagentPersistence) takeBatch() (map[string]subagent.Snapshot, map[string]transcriptWrite) {
	p.mu.Lock()
	defer p.mu.Unlock()
	trees, transcripts := p.trees, p.transcripts
	p.trees = map[string]subagent.Snapshot{}
	p.transcripts = map[string]transcriptWrite{}
	return trees, transcripts
}

func (p *subagentPersistence) retryTemporary(write func(context.Context) error) error {
	var err error
	for attempt := range subagentPersistenceMaxAttempts {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), defaultSubagentPersistenceTimeout)
		err = write(ctx)
		cancel()
		if err == nil || !session.IsTemporary(err) || attempt+1 == subagentPersistenceMaxAttempts {
			return err
		}
		base := subagentPersistenceRetryBase << attempt
		delay := base/2 + time.Duration(rand.Int64N(int64(base)))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-p.ctx.Done():
			timer.Stop()
			return p.ctx.Err()
		}
	}
	return err
}

func (p *subagentPersistence) requeue(trees map[string]subagent.Snapshot, transcripts map[string]transcriptWrite) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, snapshot := range trees {
		if _, newer := p.trees[id]; !newer {
			p.trees[id] = snapshot
		}
	}
	for id, write := range transcripts {
		if _, newer := p.transcripts[id]; !newer {
			p.transcripts[id] = write
		}
	}
}

func (p *subagentPersistence) flushBatch() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	trees, transcripts := p.takeBatch()
	failedTrees := map[string]subagent.Snapshot{}
	failedTranscripts := map[string]transcriptWrite{}
	var failures []error
	for id, snapshot := range trees {
		if p.treeStore == nil {
			continue
		}
		err := p.retryTemporary(func(ctx context.Context) error { return p.treeStore.SaveTree(ctx, id, snapshot) })
		if err != nil {
			failedTrees[id] = snapshot
			failures = append(failures, fmt.Errorf("persist subagent tree for session %s: %w", id, err))
		}
	}
	for id, write := range transcripts {
		if p.sessionStore == nil {
			continue
		}
		err := p.retryTemporary(func(ctx context.Context) error {
			return p.sessionStore.AddSubSession(ctx, write.parentID, write.session)
		})
		// Unregistered in-process parents are explicitly volatile; there is no
		// durable transcript promise to fail in that mode.
		if errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			failedTranscripts[id] = write
			failures = append(failures, fmt.Errorf("persist subagent transcript for session %s: %w", id, err))
		}
	}
	p.requeue(failedTrees, failedTranscripts)
	return errors.Join(failures...)
}

// flushNow is an explicit durability barrier for every value enqueued before
// the call. Concurrent newer values may supersede a failed covered write.
func (p *subagentPersistence) flushNow() error {
	if p == nil {
		return nil
	}
	return p.flushBatch()
}

func (p *subagentPersistence) close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	// Stop the background consumer and wait for its bounded current write. Any
	// failed value has already been requeued; do not repeat another full store
	// timeout during shutdown.
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(defaultSubagentFlushTimeout):
		return errors.New("timed out flushing subagent persistence")
	}
	p.mu.Lock()
	pending := len(p.trees) != 0 || len(p.transcripts) != 0
	p.mu.Unlock()
	if pending {
		return p.flushBatch()
	}
	return nil
}
