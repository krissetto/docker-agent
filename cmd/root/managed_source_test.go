package root

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

func TestResolveManagedSourcePath(t *testing.T) {
	workspace := t.TempDir()
	connection := filepath.Join(workspace, "sub dir")
	require.NoError(t, os.Mkdir(connection, 0o700))
	team := filepath.Join(connection, "my agent.yaml")
	require.NoError(t, os.WriteFile(team, []byte("agents: {}"), 0o600))
	resolved, relative, err := resolveManagedSourcePath(workspace, "sub dir", "./my agent.yaml")
	require.NoError(t, err)
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	canonicalTeam, err := filepath.EvalSymlinks(team)
	require.NoError(t, err)
	assert.Equal(t, canonicalWorkspace, filepath.Dir(filepath.Dir(resolved)))
	assert.Equal(t, canonicalTeam, resolved)
	assert.Equal(t, filepath.Join("sub dir", "my agent.yaml"), relative)

	outside := filepath.Join(t.TempDir(), "outside.yaml")
	require.NoError(t, os.WriteFile(outside, []byte("agents: {}"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(connection, "escape.yaml")))
	_, _, err = resolveManagedSourcePath(workspace, "sub dir", "escape.yaml")
	require.ErrorContains(t, err, "outside the sandbox workspace")
	_, _, err = resolveManagedSourcePath(workspace, "sub dir", "missing.yaml")
	require.ErrorContains(t, err, "team file not found")
	notes := filepath.Join(connection, "notes.txt")
	require.NoError(t, os.WriteFile(notes, []byte("text"), 0o600))
	_, _, err = resolveManagedSourcePath(workspace, "sub dir", "notes.txt")
	require.ErrorContains(t, err, "workspace YAML file")
}

func TestManagedSourceRegistrationPersistsAndAdvancesImmutableRevision(t *testing.T) {
	workspace := t.TempDir()
	customDir := filepath.Join(workspace, "team files")
	require.NoError(t, os.Mkdir(customDir, 0o700))
	teamPath := filepath.Join(customDir, "my agent.yaml")
	writeTeam := func(instruction string) {
		t.Helper()
		yaml := "agents:\n  root:\n    model: openai/offline-test\n    instruction: " + instruction + "\n"
		require.NoError(t, os.WriteFile(teamPath, []byte(yaml), 0o600))
	}
	writeTeam("first")
	defaultPath := filepath.Join(workspace, "default.yaml")
	require.NoError(t, os.WriteFile(defaultPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: default\n"), 0o600))
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}).managedSnapshot(t.Context(), defaultPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, nil)
	register := func(path, workingDir string) (api.SourceRegistration, config.Source, error) {
		registration, source, err := registrar(t.Context(), path, workingDir)
		if err == nil {
			if staged, ok := source.(interface{ CommitSource() error }); ok {
				err = staged.CommitSource()
			}
		}
		return registration, source, err
	}
	first, source, err := register("./my agent.yaml", "team files")
	require.NoError(t, err)
	assert.NotEmpty(t, first.SourceID)
	assert.NotEmpty(t, first.Revision)
	canonicalTeam, err := filepath.EvalSymlinks(teamPath)
	require.NoError(t, err)
	assert.Equal(t, canonicalTeam, source.Name())
	firstRevision, err := managedRead(sourceRevisionPath(state, first.SourceID, first.Revision))
	require.NoError(t, err)

	writeTeam("second")
	second, source, err := register("./my agent.yaml", "team files")
	require.NoError(t, err)
	assert.Equal(t, first.SourceID, second.SourceID, "identity follows canonical workspace-relative path")
	assert.NotEqual(t, first.Revision, second.Revision, "content changes create immutable revisions")
	secondRevision, err := managedRead(sourceRevisionPath(state, second.SourceID, second.Revision))
	require.NoError(t, err)
	assert.NotEqual(t, firstRevision, secondRevision)
	defaultRegistration, _, err := register("default.yaml", "")
	require.NoError(t, err)
	assert.Equal(t, manifest.Startup().SourceKey, defaultRegistration.SourceID, "the creation-bound source retains its existing identity")
	head, err := readSourceHead(filepath.Join(filepath.Dir(sourceRevisionPath(state, second.SourceID, second.Revision)), "current"), second.SourceID, "team files/my agent.yaml")
	require.NoError(t, err)
	assert.Equal(t, second.Revision, head.Revision)
	restored := make(map[string]string)
	fresh := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	require.NoError(t, f.restoreManagedSources(t.Context(), state, workspace, fresh))
	for _, pair := range []struct{ id, revision string }{{second.SourceID, second.Revision}, {defaultRegistration.SourceID, defaultRegistration.Revision}} {
		source, ok := fresh.Source(pair.id)
		require.True(t, ok)
		generation := source.(interface{ Generation() uint64 }).Generation()
		immutable, err := source.(interface {
			SourceForGeneration(uint64) (config.Source, error)
		}).SourceForGeneration(generation)
		require.NoError(t, err)
		revision, ok := immutable.(interface{ RevisionID() string })
		require.True(t, ok)
		assert.Equal(t, pair.revision, revision.RevisionID())
		restored[pair.id] = revision.RevisionID()
	}
	assert.Equal(t, second.Revision, restored[second.SourceID])
	assert.Equal(t, defaultRegistration.Revision, restored[defaultRegistration.SourceID])

	invalid := filepath.Join(customDir, "invalid.yaml")
	require.NoError(t, os.WriteFile(invalid, []byte("not: [valid"), 0o600))
	_, _, err = register("invalid.yaml", "team files")
	require.Error(t, err)
	_, _, err = register("missing.yaml", "team files")
	require.ErrorContains(t, err, "team file not found")
}

func TestManagedSnapshotPreservesCreationBoundTeamEditGrant(t *testing.T) {
	workspace := t.TempDir()
	team := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(team, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: test\n"), 0o600))
	flags := &runExecFlags{runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace, TeamEditMode: "restricted", TeamEditAllowedAgentIDs: []string{"root"}}}}
	manifest, err := flags.managedSnapshot(t.Context(), team, workspace)
	require.NoError(t, err)
	startup := manifest.Startup()
	assert.Equal(t, "restricted", startup.Runtime.TeamEditMode)
	assert.Equal(t, []string{"root"}, startup.Runtime.TeamEditAllowedAgentIDs)
}

func TestManagedTeamEditPersistencePublishesCASAndRestoresSavedRevision(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	physical := []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: baseline\n")
	require.NoError(t, os.WriteFile(teamPath, physical, 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	store := session.NewInMemorySessionStore()
	manager := newManagedSourceTestManager(t, f, manifest, store)
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	publish := func() (api.SourceRegistration, config.Source) {
		t.Helper()
		registration, candidate, err := registrar(t.Context(), "team.yaml", "")
		require.NoError(t, err)
		staged, ok := candidate.(interface{ CommitSource() error })
		require.True(t, ok)
		published := make(chan error, 1)
		go func() {
			published <- manager.PublishSourceWithCommit(registration.SourceID, candidate, staged.CommitSource)
		}()
		select {
		case err := <-published:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("source publication deadlocked while committing its staged head")
		}
		return registration, candidate
	}
	first, original := publish()
	persistence := original.(interface {
		TeamEditPersistence() *runtime.TeamEditPersistence
	}).TeamEditPersistence()
	require.NotNil(t, persistence)

	edited := []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: persisted edit\n")
	candidate := managedTestSource{name: teamPath, parentDir: workspace, data: edited}
	nextRevision, err := persistence.Persist(t.Context(), first.Revision, candidate)
	require.NoError(t, err)
	require.NotEqual(t, first.Revision, nextRevision)
	headPath := filepath.Join(managedSourceDir(state), sourceStorageKey(first.SourceID), "current")
	headBytes, err := managedRead(headPath)
	require.NoError(t, err)
	head, err := readSourceHead(headPath, first.SourceID, "team.yaml")
	require.NoError(t, err)
	require.Equal(t, nextRevision, head.Revision)
	oldManifest, err := managedRead(sourceRevisionPath(state, first.SourceID, first.Revision))
	require.NoError(t, err)

	loader, ok := manager.Source(first.SourceID)
	require.True(t, ok)
	generation := loader.(interface{ Generation() uint64 }).Generation()
	// A stale CAS must not change either the durable head or the active loader.
	_, err = persistence.Persist(t.Context(), first.Revision, candidate)
	require.ErrorContains(t, err, "revision conflict")
	afterHead, err := managedRead(headPath)
	require.NoError(t, err)
	assert.Equal(t, headBytes, afterHead)
	assert.Equal(t, generation, loader.(interface{ Generation() uint64 }).Generation())
	active, err := loader.(interface {
		SourceForGeneration(uint64) (config.Source, error)
	}).SourceForGeneration(generation)
	require.NoError(t, err)
	activeData, err := active.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(activeData), "persisted edit")

	secondCandidate := managedTestSource{name: teamPath, parentDir: workspace, data: []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: persisted edit two\n")}
	latestRevision, err := persistence.Persist(t.Context(), nextRevision, secondCandidate)
	require.NoError(t, err, "the same resident editor can advance its opaque expected source revision after a successful commit")
	require.NotEqual(t, nextRevision, latestRevision)

	// The YAML input did not change, so reconnect keeps the latest saved edit.
	reconnected, _ := publish()
	assert.Equal(t, latestRevision, reconnected.Revision)
	assert.Equal(t, oldManifest, mustManagedRevisionManifest(t, state, first.SourceID, first.Revision))

	fresh := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	require.NoError(t, f.restoreManagedSources(t.Context(), state, workspace, fresh))
	restored, ok := fresh.Source(first.SourceID)
	require.True(t, ok)
	currentGeneration := restored.(interface{ Generation() uint64 }).Generation()
	currentSource, err := restored.(interface {
		SourceForGeneration(uint64) (config.Source, error)
	}).SourceForGeneration(currentGeneration)
	require.NoError(t, err)
	restoredData, err := currentSource.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(restoredData), "persisted edit two")
}

type managedTestSource struct {
	name, parentDir string
	data            []byte
}

func (s managedTestSource) Name() string      { return s.name }
func (s managedTestSource) ParentDir() string { return s.parentDir }
func (s managedTestSource) Read(context.Context) ([]byte, error) {
	return append([]byte(nil), s.data...), nil
}

func mustManagedRevisionManifest(t *testing.T, state, sourceID, revision string) []byte {
	t.Helper()
	data, err := managedRead(sourceRevisionPath(state, sourceID, revision))
	require.NoError(t, err)
	return data
}

func newManagedSourceTestManager(t *testing.T, f *apiFlags, manifest *snapshot.Manifest, store session.Store) *server.SessionManager {
	t.Helper()
	digest, err := manifest.Digest()
	require.NoError(t, err)
	initial := managedFrozenSource{Source: manifest.FrozenSource(), manifest: manifest, revision: digest}
	manager := server.NewSessionManager(t.Context(), config.Sources{manifest.Startup().SourceKey: initial}, store, 0, &f.runConfig,
		server.WithSessionRuntimeFactory(func(context.Context, config.Source, string) (runtime.SessionRuntimeSupervisor, error) {
			return nil, errors.New("test runtime factory should not be called")
		}),
	)
	t.Cleanup(func() { require.NoError(t, manager.Shutdown(context.WithoutCancel(t.Context()))) })
	return manager
}

func TestManagedInputDigestTracksInstructionFiles(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	promptPath := filepath.Join(workspace, "prompt.md")
	team := []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction_file: prompt.md\n")
	require.NoError(t, os.WriteFile(teamPath, team, 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("first"), 0o600))
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	first, err := inputDigest(t.Context(), manifest, workspace)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(promptPath, []byte("second"), 0o600))
	second, err := inputDigest(t.Context(), manifest, workspace)
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "inlined local instruction dependencies remain covered by the source input fingerprint")
}

func TestManagedPersistenceRejectsChangedLocalDependencyWithoutPublishing(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	promptPath := filepath.Join(workspace, "prompt.md")
	require.NoError(t, os.WriteFile(teamPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction_file: prompt.md\n"), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("baseline prompt"), 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	state := filepath.Join(t.TempDir(), "managed")
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	registration, candidate, err := registrar(t.Context(), "team.yaml", "")
	require.NoError(t, err)
	staged := candidate.(interface{ CommitSource() error })
	require.NoError(t, manager.PublishSourceWithCommit(registration.SourceID, candidate, staged.CommitSource))
	persistence := candidate.(interface {
		TeamEditPersistence() *runtime.TeamEditPersistence
	}).TeamEditPersistence()
	beforeHead, err := managedRead(filepath.Join(managedSourceDir(state), sourceStorageKey(registration.SourceID), "current"))
	require.NoError(t, err)
	loader, ok := manager.Source(registration.SourceID)
	require.True(t, ok)
	generation := loader.(interface{ Generation() uint64 }).Generation()
	require.NoError(t, os.WriteFile(promptPath, []byte("manually changed prompt"), 0o600))
	_, err = persistence.Persist(t.Context(), registration.Revision, managedTestSource{name: teamPath, parentDir: workspace, data: []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: edit\n")})
	require.ErrorContains(t, err, "workspace source inputs changed")
	afterHead, readErr := managedRead(filepath.Join(managedSourceDir(state), sourceStorageKey(registration.SourceID), "current"))
	require.NoError(t, readErr)
	assert.Equal(t, beforeHead, afterHead)
	assert.Equal(t, generation, loader.(interface{ Generation() uint64 }).Generation())
}

func TestManagedPersistenceConcurrentCASHasSingleWinner(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(teamPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: baseline\n"), 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	register := func() (api.SourceRegistration, config.Source) {
		registration, source, err := registrar(t.Context(), "team.yaml", "")
		require.NoError(t, err)
		staged := source.(interface{ CommitSource() error })
		require.NoError(t, manager.PublishSourceWithCommit(registration.SourceID, source, staged.CommitSource))
		return registration, source
	}
	first, source := register()
	managedFrozen := source.(stagedManagedSource).managedFrozenSource
	second, _ := register()
	assert.Equal(t, first.Revision, second.Revision)
	coordinators := []*managedSourceCoordinator{
		{f: f, state: state, workspace: workspace, manager: manager},
		{f: f, state: state, workspace: workspace, manager: manager},
	}
	results := make(chan error, 2)
	for i, input := range []string{"edit one", "edit two"} {
		coordinator, input := coordinators[i], input
		go func() {
			_, err := coordinator.persist(t.Context(), managedFrozen, first.Revision, managedTestSource{name: teamPath, parentDir: workspace, data: []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: " + input + "\n")})
			results <- err
		}()
	}
	firstErr, secondErr := <-results, <-results
	if firstErr == nil {
		assert.ErrorContains(t, secondErr, "revision conflict")
	} else {
		assert.ErrorContains(t, firstErr, "revision conflict")
		require.NoError(t, secondErr)
	}
}

func TestManagedDependencyChangeCreatesRevisionAndStaleStageCannotPublish(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	promptPath := filepath.Join(workspace, "prompt.md")
	require.NoError(t, os.WriteFile(teamPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction_file: prompt.md\n"), 0o600))
	require.NoError(t, os.WriteFile(promptPath, []byte("one"), 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	register := func() (api.SourceRegistration, config.Source) {
		registration, source, err := registrar(t.Context(), "team.yaml", "")
		require.NoError(t, err)
		staged := source.(interface{ CommitSource() error })
		require.NoError(t, manager.PublishSourceWithCommit(registration.SourceID, source, staged.CommitSource))
		return registration, source
	}
	first, _ := register()
	firstManifest, err := managedRead(sourceRevisionPath(state, first.SourceID, first.Revision))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(promptPath, []byte("two"), 0o600))
	second, _ := register()
	require.NotEqual(t, first.Revision, second.Revision, "changing a referenced local instruction file creates a new source revision")
	afterFirstManifest, err := managedRead(sourceRevisionPath(state, first.SourceID, first.Revision))
	require.NoError(t, err)
	assert.Equal(t, firstManifest, afterFirstManifest, "the prior immutable source revision remains unchanged")

	// Prepare two staged candidates against the same head. The newer request wins;
	// the stale one must not move disk or publish a loader generation.
	require.NoError(t, os.WriteFile(promptPath, []byte("three"), 0o600))
	staleRegistration, staleSource, err := registrar(t.Context(), "team.yaml", "")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(promptPath, []byte("four"), 0o600))
	winnerRegistration, winnerSource, err := registrar(t.Context(), "team.yaml", "")
	require.NoError(t, err)
	winnerStage := winnerSource.(interface{ CommitSource() error })
	require.NoError(t, manager.PublishSourceWithCommit(winnerRegistration.SourceID, winnerSource, winnerStage.CommitSource))
	loader, ok := manager.Source(first.SourceID)
	require.True(t, ok)
	generation := loader.(interface{ Generation() uint64 }).Generation()
	winnerHead, err := managedRead(filepath.Join(managedSourceDir(state), sourceStorageKey(first.SourceID), "current"))
	require.NoError(t, err)
	staleStage := staleSource.(interface{ CommitSource() error })
	err = manager.PublishSourceWithCommit(staleRegistration.SourceID, staleSource, staleStage.CommitSource)
	require.ErrorContains(t, err, "revision conflict")
	afterStaleHead, err := managedRead(filepath.Join(managedSourceDir(state), sourceStorageKey(first.SourceID), "current"))
	require.NoError(t, err)
	assert.Equal(t, winnerHead, afterStaleHead)
	assert.Equal(t, generation, loader.(interface{ Generation() uint64 }).Generation())
}

func TestManagedRegistrationAndPersistenceSerializeTogether(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(teamPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: baseline\n"), 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	registration, source, err := registrar(t.Context(), "team.yaml", "")
	require.NoError(t, err)
	staged := source.(interface{ CommitSource() error })
	require.NoError(t, manager.PublishSourceWithCommit(registration.SourceID, source, staged.CommitSource))
	persistence := source.(interface {
		TeamEditPersistence() *runtime.TeamEditPersistence
	}).TeamEditPersistence()

	start := make(chan struct{})
	persisted := make(chan error, 1)
	registered := make(chan error, 1)
	go func() {
		<-start
		_, err := persistence.Persist(t.Context(), registration.Revision, managedTestSource{name: teamPath, parentDir: workspace, data: []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: concurrent edit\n")})
		persisted <- err
	}()
	go func() {
		<-start
		newRegistration, candidate, err := registrar(t.Context(), "team.yaml", "")
		if err == nil {
			stage := candidate.(interface{ CommitSource() error })
			err = manager.PublishSourceWithCommit(newRegistration.SourceID, candidate, stage.CommitSource)
		}
		registered <- err
	}()
	close(start)
	select {
	case err := <-persisted:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("team edit persistence deadlocked with source registration")
	}
	select {
	case err := <-registered:
		if err != nil {
			assert.ErrorContains(t, err, "revision conflict")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source registration deadlocked with team edit persistence")
	}
	head, err := readSourceHead(filepath.Join(managedSourceDir(state), sourceStorageKey(registration.SourceID), "current"), registration.SourceID, "team.yaml")
	require.NoError(t, err)
	loader, ok := manager.Source(registration.SourceID)
	require.True(t, ok)
	generation := loader.(interface{ Generation() uint64 }).Generation()
	active, err := loader.(interface {
		SourceForGeneration(uint64) (config.Source, error)
	}).SourceForGeneration(generation)
	require.NoError(t, err)
	activeRevision, ok := active.(interface{ RevisionID() string })
	require.True(t, ok)
	assert.Equal(t, head.Revision, activeRevision.RevisionID(), "the durable current pointer and published loader generation agree")
}

func TestManagedPersistenceRejectsNewExternalReference(t *testing.T) {
	workspace := t.TempDir()
	teamPath := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(teamPath, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: baseline\n"), 0o600))
	runtimeCfg := config.Config{WorkingDir: workspace}
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: runtimeCfg}}).managedSnapshot(t.Context(), teamPath, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: runtimeCfg}}
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	state := filepath.Join(t.TempDir(), "managed")
	registrar := f.managedSourceRegistrar(state, workspace, manager)
	registration, source, err := registrar(t.Context(), "team.yaml", "")
	require.NoError(t, err)
	staged := source.(interface{ CommitSource() error })
	require.NoError(t, manager.PublishSourceWithCommit(registration.SourceID, source, staged.CommitSource))
	persistence := source.(interface {
		TeamEditPersistence() *runtime.TeamEditPersistence
	}).TeamEditPersistence()
	candidate := managedTestSource{name: teamPath, parentDir: workspace, data: []byte("agents:\n  root:\n    model: openai/offline-test\n    sub_agents: [new-team:https://example.com/new-team.yaml]\n")}
	_, err = persistence.Persist(t.Context(), registration.Revision, candidate)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "new-team")
	head, err := readSourceHead(filepath.Join(managedSourceDir(state), sourceStorageKey(registration.SourceID), "current"), registration.SourceID, "team.yaml")
	require.NoError(t, err)
	assert.Equal(t, registration.Revision, head.Revision)
}

func TestManagedInputDigestAllowsTrustedStartupSourceOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "startup.yaml")
	require.NoError(t, os.WriteFile(outside, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: startup\n"), 0o600))
	manifest, err := snapshot.Snapshot(t.Context(), config.NewFileSource(outside), snapshot.Options{Startup: snapshot.Startup{Workspace: workspace, SourceKey: "trusted-startup"}})
	require.NoError(t, err)
	_, err = inputDigest(t.Context(), manifest, workspace)
	require.NoError(t, err, "the immutable creation-bound startup file remains an internal source, not a user-selected workspace path")
}

func TestManagedStartupSourceOutsideWorkspaceUsesTrustedInternalSentinel(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "startup.yaml")
	require.NoError(t, os.WriteFile(outside, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: startup\n"), 0o600))
	manifest, err := (&runExecFlags{runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}).managedSnapshot(t.Context(), outside, workspace)
	require.NoError(t, err)
	f := &apiFlags{manifest: manifest, runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}
	state := filepath.Join(t.TempDir(), "managed")
	f.managedState = state
	manager := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	require.NoError(t, f.initializeManagedStartupSource(t.Context(), manager))
	id := manifest.Startup().SourceKey
	identityData, err := managedRead(filepath.Join(managedSourceDir(state), sourceStorageKey(id), "identity.json"))
	require.NoError(t, err)
	var identity persistedSourceIdentity
	require.NoError(t, json.Unmarshal(identityData, &identity))
	assert.Equal(t, "@startup", identity.Path)
	fresh := newManagedSourceTestManager(t, f, manifest, session.NewInMemorySessionStore())
	require.NoError(t, f.restoreManagedSources(t.Context(), state, workspace, fresh))
	restored, ok := fresh.Source(id)
	require.True(t, ok)
	generation := restored.(interface{ Generation() uint64 }).Generation()
	active, err := restored.(interface {
		SourceForGeneration(uint64) (config.Source, error)
	}).SourceForGeneration(generation)
	require.NoError(t, err)
	persistence, ok := active.(interface {
		TeamEditPersistence() *runtime.TeamEditPersistence
	})
	require.True(t, ok)
	require.NotNil(t, persistence.TeamEditPersistence())
}

func TestManagedSnapshotForPreservesStartupFlagsAndEnvNormalization(t *testing.T) {
	workspace := t.TempDir()
	team := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(team, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: baseline\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "team.env"), []byte("TEAM_VALUE=1\n"), 0o600))
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workspace, EnvFiles: []string{"team.env"}}}

	manifest, err := managedSnapshotFor(t.Context(), runConfig, []string{"openai/override-model"}, true, team, workspace)
	require.NoError(t, err)
	startup := manifest.Startup()
	assert.True(t, startup.Snapshots)
	assert.Equal(t, []string{"openai/override-model"}, startup.ModelOverrides)
	assert.Equal(t, workspace, startup.Runtime.WorkingDir)
	// The env-file normalization happens on the caller's config, not a copy, and the snapshot records it.
	assert.Equal(t, []string{filepath.Join(workspace, "team.env")}, runConfig.EnvFiles)
	assert.Equal(t, []string{filepath.Join(workspace, "team.env")}, startup.Runtime.EnvFiles)

	disabled, err := managedSnapshotFor(t.Context(), runConfig, nil, false, team, workspace)
	require.NoError(t, err)
	assert.False(t, disabled.Startup().Snapshots)
	assert.Empty(t, disabled.Startup().ModelOverrides)
}
