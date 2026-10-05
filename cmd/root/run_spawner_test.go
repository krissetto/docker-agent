package root

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui"
)

// TestSpawnerOwnsRuntimeForAnotherWorkingDir pins the tab/working-directory
// contract: a tab opened in a directory other than the running runtime's gets
// a runtime of its own whose toolsets operate in that directory, while a tab
// in the same directory borrows the shared session registry.
func TestSpawnerOwnsRuntimeForAnotherWorkingDir(t *testing.T) {
	baseDir := t.TempDir()
	otherDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(otherDir, "marker.txt"), []byte("here"), 0o644))

	agentFile := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: You are a test agent.
    toolsets:
      - type: filesystem
`), 0o644))
	agentSource, err := sources.Resolve(agentFile, nil)
	require.NoError(t, err)

	flags := &runExecFlags{sessionDB: filepath.Join(t.TempDir(), "session.db")}
	flags.runConfig = config.RuntimeConfig{
		WorkingDir:          baseDir,
		EnvProviderForTests: environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"}),
	}
	loadResult, err := flags.loadAgentFrom(t.Context(), flags.loadTeamRequest(agentSource))
	require.NoError(t, err)
	t.Cleanup(func() { stopToolSets(context.WithoutCancel(t.Context()), loadResult.Team) })

	services, sessions, _, cleanup, err := (&localBackend{flags: flags, agentSource: agentSource}).CreateSession(t.Context(), loadResult, flags.createSessionRequest(baseDir))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	spawner := flags.createSessionSpawner(agentSource, services, sessions)
	require.NotNil(t, spawner)

	borrowed, err := spawner(t.Context(), baseDir)
	require.NoError(t, err)
	assert.Equal(t, tui.RuntimeBorrowed, borrowed.Ownership, "same directory keeps the shared runtime")
	assert.Same(t, sessions, borrowed.App.SessionRuntime())

	owned, err := spawner(t.Context(), otherDir)
	require.NoError(t, err)
	t.Cleanup(owned.Cleanup)
	assert.Equal(t, tui.RuntimeOwned, owned.Ownership, "another directory gets its own runtime")
	require.NotNil(t, owned.Cleanup)
	assert.NotSame(t, sessions, owned.App.SessionRuntime())
	assert.Equal(t, otherDir, owned.Session.WorkingDir)

	// The owned runtime's filesystem tools are rooted in the new directory.
	toolDefs, err := owned.App.CurrentAgentTools(t.Context())
	require.NoError(t, err)
	var listDir *tools.Tool
	for i := range toolDefs {
		if toolDefs[i].Name == "list_directory" {
			listDir = &toolDefs[i]
		}
	}
	require.NotNil(t, listDir, "the spawned runtime must expose the filesystem toolset")
	result, err := listDir.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "list_directory", Arguments: `{"path":"."}`}}, nil)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "marker.txt", "tools must operate in the tab's working directory")
}

func TestLeanSessionSpawnerWorkingDirectoryIsolation(t *testing.T) {
	baseDir, otherDir := t.TempDir(), t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	for dir, marker := range map[string]string{baseDir: "original.txt", otherDir: "spawned.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, marker), []byte(marker), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "LEAN_TEST_PROMPT.md"), []byte(marker), 0o600))
	}
	agentFile := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: You are a test agent.
    add_prompt_files:
      - LEAN_TEST_PROMPT.md
    toolsets:
      - type: filesystem
`), 0o600))
	source, err := sources.Resolve(agentFile, nil)
	require.NoError(t, err)
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = baseDir
	flags.runConfig.EnvProviderForTests = environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "sk-test"})
	loaded, err := flags.loadAgentFrom(t.Context(), flags.loadTeamRequest(source))
	require.NoError(t, err)
	t.Cleanup(func() { stopToolSets(context.WithoutCancel(t.Context()), loaded.Team) })
	store := session.NewInMemorySessionStore()
	rt, sess, err := flags.createLocalRuntimeAndSession(t.Context(), loaded, flags.createSessionRequest(baseDir), store)
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sessions := owner.Runtime()
	original := app.New(t.Context(), sessions, sess, sessionSessionBinding(t.Context(), rt, sess), app.WithRuntimeServices(rt))
	t.Cleanup(original.Close)
	spawn := leanSessionSpawner(flags.createSessionSpawner(source, rt, sessions), store)
	other, cleanup, err := spawn(t.Context(), otherDir, nil)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	t.Cleanup(cleanup)
	t.Cleanup(other.Close)
	require.NotSame(t, sessions, other.SessionRuntime())

	check := func(a *app.App, dir, want, absent string) {
		t.Helper()
		defs, err := a.CurrentAgentTools(t.Context())
		require.NoError(t, err)
		var listDir *tools.Tool
		for i := range defs {
			if defs[i].Name == "list_directory" {
				listDir = &defs[i]
			}
		}
		require.NotNil(t, listDir)
		result, err := listDir.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "list_directory", Arguments: `{"path":"."}`}}, nil)
		require.NoError(t, err)
		assert.Contains(t, result.Output, want)
		assert.NotContains(t, result.Output, absent)
		breakdown, err := a.ContextBreakdown(t.Context())
		require.NoError(t, err)
		require.Len(t, breakdown.PromptFileItems, 1)
		assert.Equal(t, filepath.Join(dir, "LEAN_TEST_PROMPT.md"), breakdown.PromptFileItems[0].Path)
	}
	check(original, baseDir, "original.txt", "spawned.txt")
	check(other, otherDir, "spawned.txt", "original.txt")
	check(original, baseDir, "original.txt", "spawned.txt")
	check(other, otherDir, "spawned.txt", "original.txt")
	cleanup()
	cleanup()
	check(original, baseDir, "original.txt", "spawned.txt")
	borrowed, borrowedCleanup, err := spawn(t.Context(), baseDir, nil)
	require.NoError(t, err)
	t.Cleanup(borrowed.Close)
	assert.Nil(t, borrowedCleanup)
	assert.Same(t, sessions, borrowed.SessionRuntime())
	assert.Equal(t, baseDir, flags.runConfig.WorkingDir)
	assert.Equal(t, baseDir, original.Session().WorkingDir)
	finalCwd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, cwd, finalCwd, "spawning must not change process cwd")
}

func TestLeanSessionSpawnerForkPreservesBindingAndHistory(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root instructions", agent.WithModel(rootTestProvider{})),
		agent.New("worker", "worker instructions", agent.WithModel(rootTestProvider{})),
	)), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sessions := owner.Runtime()
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = t.TempDir()
	source := session.New(session.WithAgentName("worker"), session.WithWorkingDir(flags.runConfig.WorkingDir), session.WithSafetyPolicy(session.SafetyPolicyStrict))
	source.AgentModelOverrides = map[string]string{"worker": "test/override"}
	source.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "fork this history"}})
	before := source.Clone()
	spawn := leanSessionSpawner(flags.createSessionSpawner(nil, rt, sessions), store)
	fork, cleanup, err := spawn(t.Context(), "ignored-for-fork", source)
	require.NoError(t, err)
	t.Cleanup(fork.Close)
	assert.Nil(t, cleanup)
	assert.NotEqual(t, source.ID, fork.Session().ID)
	assert.Equal(t, fork.Session().ID, fork.SessionHandle().ID())
	assert.Equal(t, "worker", fork.SessionHandle().AgentName())
	assert.Equal(t, "test/override", fork.Binding().Model)
	assert.Equal(t, "test/override", fork.SessionHandle().Metadata().Model)
	assert.Equal(t, source.WorkingDir, fork.Session().WorkingDir)
	assert.Equal(t, session.SafetyPolicyStrict, fork.Session().GetSafetyPolicy())
	assert.Contains(t, fork.PlainTextTranscript(), "fork this history")
	persisted, err := store.GetSession(t.Context(), fork.Session().ID)
	require.NoError(t, err)
	assert.Equal(t, "worker", persisted.AgentName)
	assert.Equal(t, "test/override", persisted.AgentModelOverrides["worker"])
	assert.Equal(t, before, source, "forking must leave the source untouched")

	// Ownership is conveyed separately from the App: only the owned result
	// receives an idempotent shutdown callback, including failed admission.
	cleanupCalls := 0
	canonical := flags.createSessionSpawner(nil, rt, sessions)
	ownedSpawn := leanSessionSpawner(func(ctx context.Context, dir string) (tui.SpawnedSession, error) {
		spawned, err := canonical(ctx, dir)
		spawned.Ownership = tui.RuntimeOwned
		spawned.Cleanup = func() { cleanupCalls++ }
		return spawned, err
	}, store)
	owned, ownedCleanup, err := ownedSpawn(t.Context(), flags.runConfig.WorkingDir, nil)
	require.NoError(t, err)
	t.Cleanup(owned.Close)
	require.NotNil(t, ownedCleanup)
	ownedCleanup()
	ownedCleanup()
	assert.Equal(t, 1, cleanupCalls)
	invalidSource := session.New(session.WithAgentName("missing"), session.WithWorkingDir(flags.runConfig.WorkingDir))
	failed, failedCleanup, err := ownedSpawn(t.Context(), "", invalidSource)
	require.ErrorContains(t, err, "failed to bind")
	assert.Nil(t, failed)
	assert.Nil(t, failedCleanup)
	assert.Equal(t, 2, cleanupCalls, "failed fork admission must clean up its owned runtime")
}

type leanFailingSessionStore struct {
	session.Store

	err error
}

func (s leanFailingSessionStore) AddSession(context.Context, *session.Session) error {
	return s.err
}

func TestLeanSessionRestorerLazyIdentityAndCleanup(t *testing.T) {
	store := session.NewInMemorySessionStore()
	workingDir := t.TempDir()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root instructions", agent.WithModel(rootTestProvider{})),
		agent.New("worker", "worker instructions", agent.WithModel(rootTestProvider{})),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(workingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sessions := owner.Runtime()
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = workingDir
	initialSession := session.New(session.WithAgentName("root"), session.WithWorkingDir(workingDir))
	initial := app.New(t.Context(), sessions, initialSession, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
	t.Cleanup(initial.Close)
	stored := session.New(session.WithAgentName("worker"), session.WithWorkingDir(workingDir))
	stored.AgentModelOverrides = map[string]string{"worker": "test/override"}
	stored.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "restore this history"}})
	require.NoError(t, store.AddSession(t.Context(), stored))
	before := stored.Clone()
	spawns, cleanups := 0, 0
	owned := false
	canonical := flags.createSessionSpawner(nil, rt, sessions)
	restore := leanSessionRestorer(func(ctx context.Context, dir string) (tui.SpawnedSession, error) {
		spawns++
		assert.Equal(t, workingDir, dir, "session provenance wins before runtime admission")
		spawned, err := canonical(ctx, dir)
		if owned {
			spawned.Ownership = tui.RuntimeOwned
		}
		spawned.Cleanup = func() { cleanups++ }
		return spawned, err
	}, store, initial)
	assert.Zero(t, spawns, "cold restore metadata must not hydrate any runtime")
	reused, cleanup, err := restore(t.Context(), initialSession.ID, "ignored")
	require.NoError(t, err)
	assert.Same(t, initial, reused)
	assert.Nil(t, cleanup)
	assert.Zero(t, spawns, "the initial session must not create a duplicate runtime")

	restored, cleanup, err := restore(t.Context(), stored.ID, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(restored.Close)
	assert.Equal(t, 1, spawns)
	assert.Nil(t, cleanup, "borrowed cleanup must never escape to a viewer")
	assert.Zero(t, cleanups)
	assert.Same(t, sessions, restored.SessionRuntime())
	assert.Equal(t, stored.ID, restored.SessionHandle().ID(), "restore keeps identity rather than branching")
	assert.Equal(t, "worker", restored.SessionHandle().AgentName())
	assert.Equal(t, "test/override", restored.Binding().Model)
	assert.Equal(t, "test/override", restored.SessionHandle().Metadata().Model)
	assert.Equal(t, workingDir, restored.Session().WorkingDir)
	assert.Contains(t, restored.PlainTextTranscript(), "restore this history")
	assert.Equal(t, before, stored)

	// Old sessions without explicit metadata recover the recorded tab
	// workspace and pinned agent attribute before immutable admission.
	legacy := session.New()
	legacy.SetAttribute(runtime.SessionAgentAttribute, "worker")
	legacy.AgentModelOverrides = map[string]string{"worker": "test/legacy"}
	require.NoError(t, store.AddSession(t.Context(), legacy))
	owned = true
	restoredLegacy, ownedCleanup, err := restore(t.Context(), legacy.ID, workingDir)
	require.NoError(t, err)
	t.Cleanup(restoredLegacy.Close)
	require.NotNil(t, ownedCleanup)
	assert.Equal(t, legacy.ID, restoredLegacy.SessionHandle().ID())
	assert.Equal(t, "worker", restoredLegacy.SessionHandle().AgentName())
	assert.Equal(t, "test/legacy", restoredLegacy.Binding().Model)
	assert.Equal(t, workingDir, restoredLegacy.Session().WorkingDir)
	assert.Empty(t, legacy.WorkingDir, "normalizing the loaded snapshot must not mutate its stored source")
	ownedCleanup()
	ownedCleanup()
	assert.Equal(t, 1, cleanups)

	invalid := session.New(session.WithAgentName("missing"), session.WithWorkingDir(workingDir))
	require.NoError(t, store.AddSession(t.Context(), invalid))
	failed, failedCleanup, err := restore(t.Context(), invalid.ID, workingDir)
	require.ErrorContains(t, err, "failed to bind")
	assert.Nil(t, failed)
	assert.Nil(t, failedCleanup)
	assert.Equal(t, 2, cleanups, "partial restore failure cleans only its owned runtime")
	assert.Equal(t, initialSession.ID, initial.SessionHandle().ID())
}

type leanRestoreSessionStore struct {
	session.Store

	sess *session.Session
	err  error
}

func (s leanRestoreSessionStore) GetSession(context.Context, string) (*session.Session, error) {
	return s.sess, s.err
}

func TestLeanSessionRestorerErrors(t *testing.T) {
	store := session.NewInMemorySessionStore()
	noWorkspace := session.New(session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), noWorkspace))
	missingWorkspace := session.New(session.WithAgentName("root"), session.WithWorkingDir(filepath.Join(t.TempDir(), "missing")))
	require.NoError(t, store.AddSession(t.Context(), missingWorkspace))
	notDirectory := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("not a directory"), 0o600))
	fileWorkspace := session.New(session.WithAgentName("root"), session.WithWorkingDir(notDirectory))
	require.NoError(t, store.AddSession(t.Context(), fileWorkspace))
	want := errors.New("store unavailable")
	for _, tc := range []struct {
		name  string
		store session.Store
		id    string
		want  error
		text  string
	}{
		{name: "missing session", store: store, id: "missing", want: session.ErrNotFound},
		{name: "no store", id: "missing", text: "no session store"},
		{name: "missing provenance", store: store, id: noWorkspace.ID, want: session.ErrWorkingDirUnavailable},
		{name: "stale workspace", store: store, id: missingWorkspace.ID, want: os.ErrNotExist},
		{name: "non-directory workspace", store: store, id: fileWorkspace.ID, text: "not a directory"},
		{name: "store failure", store: leanRestoreSessionStore{Store: store, err: want}, id: "saved", want: want},
		{name: "wrong identity", store: leanRestoreSessionStore{Store: store, sess: noWorkspace}, id: "different", text: "different restored session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := leanSessionRestorer(func(context.Context, string) (tui.SpawnedSession, error) {
				t.Fatal("invalid restore must fail before spawning")
				return tui.SpawnedSession{}, nil
			}, tc.store, nil)
			a, cleanup, err := restore(t.Context(), tc.id, "")
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			} else {
				require.ErrorContains(t, err, tc.text)
			}
			assert.Nil(t, a)
			assert.Nil(t, cleanup)
		})
	}
	t.Run("remote capability", func(t *testing.T) {
		_, _, err := leanSessionRestorer(nil, store, nil)(t.Context(), "saved", "")
		require.ErrorIs(t, err, runtime.ErrUnsupported)
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := leanSessionRestorer(nil, nil, nil)(ctx, "saved", "")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestLeanSessionSpawnerErrorsAndCleanup(t *testing.T) {
	t.Run("canceled before spawn", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		spawn := leanSessionSpawner(func(context.Context, string) (tui.SpawnedSession, error) {
			t.Fatal("must reject before spawning")
			return tui.SpawnedSession{}, nil
		}, nil)
		_, _, err := spawn(ctx, "", nil)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("store failure", func(t *testing.T) {
		want := errors.New("store failure")
		spawn := leanSessionSpawner(func(context.Context, string) (tui.SpawnedSession, error) {
			t.Fatal("must save the fork before spawning")
			return tui.SpawnedSession{}, nil
		}, leanFailingSessionStore{Store: session.NewInMemorySessionStore(), err: want})
		_, _, err := spawn(t.Context(), "", session.New())
		require.ErrorIs(t, err, want)
	})
	t.Run("remote capability", func(t *testing.T) {
		spawn := leanSessionSpawner((&remoteBackend{}).Spawner(nil, nil), nil)
		a, cleanup, err := spawn(t.Context(), t.TempDir(), nil)
		require.ErrorIs(t, err, runtime.ErrUnsupported)
		assert.Nil(t, a)
		assert.Nil(t, cleanup)
	})
	t.Run("fork requires store", func(t *testing.T) {
		spawn := leanSessionSpawner(func(context.Context, string) (tui.SpawnedSession, error) {
			t.Fatal("must reject before spawning")
			return tui.SpawnedSession{}, nil
		}, nil)
		_, _, err := spawn(t.Context(), "", session.New())
		require.ErrorContains(t, err, "no session store")
	})
	for name, ownership := range map[string]bool{"owned": true, "borrowed": false} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			want := errors.New("spawn failure")
			spawn := leanSessionSpawner(func(context.Context, string) (tui.SpawnedSession, error) {
				spawned := tui.SpawnedSession{Ownership: tui.RuntimeBorrowed, Cleanup: func() { calls++ }}
				if ownership {
					spawned.Ownership = tui.RuntimeOwned
				}
				return spawned, want
			}, nil)
			a, cleanup, err := spawn(t.Context(), "", nil)
			require.ErrorIs(t, err, want)
			assert.Nil(t, a)
			assert.Nil(t, cleanup)
			if ownership {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
		})
	}
}
