package server

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
)

var sourceRetrySchedule = []time.Duration{2 * time.Second, 15 * time.Second, 70 * time.Second}

type sourceLoader struct {
	inner           config.Source
	refreshInterval time.Duration

	mu   sync.RWMutex
	data []byte
	err  error
	// generation counts successful loads that changed the configuration
	// bytes. Consumers that build state from the source (session runtimes)
	// key it by generation so a refreshed agent definition is picked up by
	// the next session without disturbing sessions on the previous one.
	generation uint64
}

func newSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	sl := &sourceLoader{
		inner:           inner,
		refreshInterval: refreshInterval,
	}

	sl.load(ctx)

	if sl.hasError() {
		go sl.retryStartup(ctx)
	}

	if refreshInterval > 0 {
		go sl.refreshLoop(ctx)
	}

	return sl
}

func (sl *sourceLoader) Name() string {
	return sl.inner.Name()
}

func (sl *sourceLoader) ParentDir() string {
	return sl.inner.ParentDir()
}

func (sl *sourceLoader) Read(_ context.Context) ([]byte, error) {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.data, sl.err
}

func (sl *sourceLoader) load(ctx context.Context) {
	data, err := sl.inner.Read(ctx)

	sl.mu.Lock()
	defer sl.mu.Unlock()

	if err != nil {
		// Only log errors, keep previous data if available
		slog.WarnContext(ctx, "Failed to refresh source",
			"source", sl.inner.Name(),
			"error", err)
		// Only update error if we don't have data yet
		if len(sl.data) == 0 {
			sl.err = err
		}
	} else {
		if sl.err != nil || !bytes.Equal(sl.data, data) {
			sl.generation++
		}
		sl.data = data
		sl.err = nil
	}
}

// Generation returns the current configuration generation (see the field
// doc). It is 0 until the source has been read successfully once.
func (sl *sourceLoader) Generation() uint64 {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.generation
}

func (sl *sourceLoader) hasError() bool {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.err != nil
}

func (sl *sourceLoader) retryStartup(ctx context.Context) {
	for _, delay := range sourceRetrySchedule {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		sl.load(ctx)
		if !sl.hasError() {
			return
		}
	}
}

func (sl *sourceLoader) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(sl.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sl.load(ctx)
		}
	}
}
