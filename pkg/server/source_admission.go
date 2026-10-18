package server

import (
	"errors"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
)

// RegisterSource adds one validated immutable source revision to the existing
// source and per-source session runtime registries. Runtime ownership stays
// with the existing API process and shared session store.
func (sm *SessionManager) RegisterSource(sourceID string, source config.Source) error {
	return sm.PublishSourceWithCommit(sourceID, source, nil)
}

// PublishSourceWithCommit serializes the trusted durable source head and the
// in-memory current generation. Candidate IO/validation happens before the
// source-map lock; commit failure leaves the current generation untouched.
func (sm *SessionManager) PublishSourceWithCommit(sourceID string, source config.Source, commit func() error) error {
	if sourceID == "" || source == nil {
		return errors.New("source identity and source are required")
	}
	if sm.sessionRuntimeFactory == nil {
		return errors.New("dynamic sources require a session runtime factory")
	}
	sm.sourcePublicationMu.Lock()
	defer sm.sourcePublicationMu.Unlock()

	sm.sourceMu.RLock()
	existing := sm.Sources[sourceID]
	sm.sourceMu.RUnlock()
	data, err := source.Read(sm.serverCtx)
	if err != nil {
		return err
	}
	var loader *sourceLoader
	var router *workspaceSessionRuntimes
	if existing != nil {
		var ok bool
		loader, ok = existing.(*sourceLoader)
		if !ok {
			return errors.New("registered source cannot accept immutable revisions")
		}
		if loader.Name() != source.Name() || loader.ParentDir() != source.ParentDir() {
			return errors.New("source revisions must preserve canonical source name and directory")
		}
		if commit != nil {
			if err := commit(); err != nil {
				return err
			}
		}
		loader.ReplaceSourceData(source, data)
		return nil
	}
	loader = newSourceLoaderFromData(source, data)
	router = newWorkspaceSessionRuntimes(sm.serverCtx, loader, sm.sessionRuntimeFactory, sm.sessionStore)
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	sm.sourceMu.Lock()
	sm.Sources[sourceID] = loader
	sm.sessionRegistries[sourceID] = router
	sm.ownedRegistries = append(sm.ownedRegistries, router)
	sm.sourceMu.Unlock()
	return nil
}

func (sm *SessionManager) sourceSnapshot() config.Sources {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
	out := make(config.Sources, len(sm.Sources))
	for name, source := range sm.Sources {
		out[name] = source
	}
	return out
}

func (sm *SessionManager) sourceNames() []string {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
	out := make([]string, 0, len(sm.sessionRegistries))
	for name := range sm.sessionRegistries {
		out = append(out, name)
	}
	return out
}

func (sm *SessionManager) sourceRegistry(source string) (runtime.SessionRuntime, bool) {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
	registry, ok := sm.sessionRegistries[source]
	return registry, ok
}

func (sm *SessionManager) sourceRegistryCount() int {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
	return len(sm.sessionRegistries)
}

// Source returns a registered source by its opaque session-routing identity.
func (sm *SessionManager) Source(sourceID string) (config.Source, bool) {
	sm.sourceMu.RLock()
	defer sm.sourceMu.RUnlock()
	source, ok := sm.Sources[sourceID]
	return source, ok
}
