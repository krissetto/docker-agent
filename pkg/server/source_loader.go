package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
)

var sourceRetrySchedule = []time.Duration{2 * time.Second, 15 * time.Second, 70 * time.Second}

// sourceLoader must keep satisfying config.EncryptedConfigSource: it decorates
// every agent source in API-server mode, and teamloader discovers the agent
// config envelope through a type assertion, so dropping the method would
// silently stop forwarding it to the models gateway rather than fail to build.
var _ config.EncryptedConfigSource = (*sourceLoader)(nil)

type sourceLoader struct {
	inner           config.Source
	name            string
	parentDir       string
	refreshInterval time.Duration

	mu        sync.RWMutex
	updateMu  sync.Mutex
	data      []byte
	err       error
	revision  string
	revisions map[uint64]config.Source
	// generation counts successful loads that changed the configuration
	// bytes. Consumers that build state from the source (session runtimes)
	// key it by generation so a refreshed agent definition is picked up by
	// the next session without disturbing sessions on the previous one.
	generation uint64
}

func newSourceLoaderFromData(source config.Source, data []byte) *sourceLoader {
	sl := &sourceLoader{inner: source, name: source.Name(), parentDir: source.ParentDir(), revision: sourceRevision(source), data: bytes.Clone(data), generation: 1}
	sl.recordRevisionLocked(source, data)
	return sl
}

func newSourceLoader(ctx context.Context, inner config.Source, refreshInterval time.Duration) *sourceLoader {
	sl := &sourceLoader{
		inner:           inner,
		name:            inner.Name(),
		parentDir:       inner.ParentDir(),
		revision:        sourceRevision(inner),
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

func (sl *sourceLoader) Name() string { return sl.name }

func (sl *sourceLoader) ParentDir() string { return sl.parentDir }

func (sl *sourceLoader) Read(_ context.Context) ([]byte, error) {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return bytes.Clone(sl.data), sl.err
}

// EncryptedConfig forwards the inner source's captured agent config envelope so
// this caching decorator stays transparent to callers that type-assert for
// [config.EncryptedConfigSource] — chiefly teamloader, which adopts the value
// into RuntimeConfig.EncryptedConfig so it is forwarded to a trusted Docker
// models gateway on every model request.
//
// Without this passthrough the capability is silently lost in API-server mode
// (`docker agent serve api`, i.e. Docker Desktop): NewSessionManager wraps every
// source in a sourceLoader, so teamloader's assertion fails, no config envelope
// is forwarded, and the gateway's prompt verification degrades to a no-op — it
// fails open on a request that carries no config, so nothing surfaces as an
// error. The CLI path is unaffected because it hands the source over undecorated.
// See also the identical passthrough on the HCL decorator (pkg/config/hcl).
//
// It reads through to the inner source rather than caching the value: the config
// envelope is refreshed by the very same Read that refreshLoop performs, so a
// tag repushed under a new signature is picked up without a restart. Returns ""
// when the inner source does not support the capability.
func (sl *sourceLoader) EncryptedConfig() string {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	if ecs, ok := sl.inner.(config.EncryptedConfigSource); ok {
		return ecs.EncryptedConfig()
	}
	return ""
}

func (sl *sourceLoader) load(ctx context.Context) {
	sl.updateMu.Lock()
	defer sl.updateMu.Unlock()
	sl.mu.RLock()
	inner := sl.inner
	sl.mu.RUnlock()
	data, err := inner.Read(ctx)

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
		sl.data = bytes.Clone(data)
		sl.err = nil
		sl.recordRevisionLocked(sl.inner, data)
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

// ResolveSource preserves a frozen source graph supplied by a Kit registration.
// Ordinary local/remote sources continue through the server's standard resolver.
func (sl *sourceLoader) ResolveSource(ref string, env environment.Provider) (config.Source, error) {
	sl.mu.RLock()
	inner := sl.inner
	sl.mu.RUnlock()
	if graph, ok := inner.(interface {
		ResolveSource(string, environment.Provider) (config.Source, error)
	}); ok {
		return graph.ResolveSource(ref, env)
	}
	return sources.Resolve(ref, env)
}

type loadedRevisionSource struct {
	name, parentDir, encrypted string
	data                       []byte
	resolver                   interface {
		ResolveSource(string, environment.Provider) (config.Source, error)
	}
}

func (s loadedRevisionSource) Name() string                         { return s.name }
func (s loadedRevisionSource) ParentDir() string                    { return s.parentDir }
func (s loadedRevisionSource) Read(context.Context) ([]byte, error) { return bytes.Clone(s.data), nil }
func (s loadedRevisionSource) EncryptedConfig() string              { return s.encrypted }
func (s loadedRevisionSource) ResolveSource(ref string, env environment.Provider) (config.Source, error) {
	if s.resolver != nil {
		return s.resolver.ResolveSource(ref, env)
	}
	return sources.Resolve(ref, env)
}

func (sl *sourceLoader) recordRevisionLocked(source config.Source, data []byte) {
	if sl.revisions == nil {
		sl.revisions = make(map[uint64]config.Source)
	}
	var encrypted string
	if value, ok := source.(config.EncryptedConfigSource); ok {
		encrypted = value.EncryptedConfig()
	}
	resolver, _ := source.(interface {
		ResolveSource(string, environment.Provider) (config.Source, error)
	})
	if sourceRevision(source) != "" {
		sl.revisions[sl.generation] = source
	} else {
		sl.revisions[sl.generation] = loadedRevisionSource{name: sl.name, parentDir: sl.parentDir, encrypted: encrypted, data: bytes.Clone(data), resolver: resolver}
	}
	for len(sl.revisions) > 32 {
		oldest := sl.generation
		for generation := range sl.revisions {
			if generation < oldest {
				oldest = generation
			}
		}
		delete(sl.revisions, oldest)
	}
}

func (sl *sourceLoader) SourceForGeneration(generation uint64) (config.Source, error) {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	if source := sl.revisions[generation]; source != nil {
		return source, nil
	}
	if generation == sl.generation {
		var encrypted string
		if source, ok := sl.inner.(config.EncryptedConfigSource); ok {
			encrypted = source.EncryptedConfig()
		}
		return loadedRevisionSource{name: sl.name, parentDir: sl.parentDir, encrypted: encrypted, data: bytes.Clone(sl.data), resolver: sourceResolver(sl.inner)}, nil
	}
	return nil, errors.New("source generation is no longer available")
}

func sourceResolver(source config.Source) interface {
	ResolveSource(string, environment.Provider) (config.Source, error)
} {
	resolver, _ := source.(interface {
		ResolveSource(string, environment.Provider) (config.Source, error)
	})
	return resolver
}

// ReplaceSource installs a freshly validated immutable revision without
// disturbing runtimes already indexed at the previous generation.
func (sl *sourceLoader) ReplaceSource(ctx context.Context, source config.Source) error {
	if source == nil {
		return errors.New("source is required")
	}
	if source.Name() != sl.name || source.ParentDir() != sl.parentDir {
		return errors.New("source revisions must preserve canonical source name and directory")
	}
	data, err := source.Read(ctx)
	if err != nil {
		return err
	}
	sl.ReplaceSourceData(source, data)
	return nil
}

func (sl *sourceLoader) ReplaceSourceData(source config.Source, data []byte) {
	sl.updateMu.Lock()
	defer sl.updateMu.Unlock()
	sl.mu.Lock()
	defer sl.mu.Unlock()
	revision := sourceRevision(source)
	if sl.err != nil || !bytes.Equal(sl.data, data) || revision != sl.revision {
		sl.generation++
	}
	sl.inner = source
	sl.revision = revision
	sl.data = bytes.Clone(data)
	sl.err = nil
	sl.recordRevisionLocked(source, data)
}

type immutableSourceRevision interface{ RevisionID() string }

func sourceRevision(source config.Source) string {
	if revision, ok := source.(immutableSourceRevision); ok {
		return revision.RevisionID()
	}
	return ""
}
