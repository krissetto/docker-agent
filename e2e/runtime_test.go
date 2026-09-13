package e2e_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/host/turn"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

func TestRuntime_OpenAI_Basic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := sources.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	require.NoError(t, err)

	rt, err := runtime.New(t.Context(), team)
	require.NoError(t, err)

	supervisor := ownTestRuntime(t, rt)
	handle, err := supervisor.Runtime().CreateSession(ctx,
		session.New(session.WithNonInteractive(true)), runtime.SessionBinding{})
	require.NoError(t, err)
	sess, err := runTestTurn(ctx, handle, "What's 2+2?")
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	assert.Equal(t, "2 + 2 equals 4.", response)
	// Title generation is now handled by pkg/app or pkg/server, not the runtime
}

// TestRuntime_MultiAgent_SessionReload verifies that a multi-agent
// task transfer does not corrupt the parent session's persisted
// history. Before the fix (PR #2058), sub-agent streaming events
// (AgentChoiceEvent, AgentChoiceReasoningEvent) were processed by the
// PersistentRuntime against the parent session's store, creating orphan
// assistant messages. On session reload and follow-up, these orphan messages
// corrupt the message sequence sent to the model, causing API errors.
//
// The test:
//  1. Runs a first turn where the root agent delegates via transfer_task
//  2. Persists the session to a SQLite store
//  3. Reloads the session from the store
//  4. Runs a second turn (follow-up) on the reloaded session
//  5. Asserts the follow-up succeeds without errors
func TestRuntime_MultiAgent_SessionReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := sources.Resolve("testdata/multi_transfer.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	require.NoError(t, err)

	// Use a SQLite store so we test real persistence and reload.
	dbPath := filepath.Join(t.TempDir(), "session.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	rt, err := runtime.New(t.Context(), team, runtime.WithSessionStore(store))
	require.NoError(t, err)

	supervisor := ownTestRuntime(t, rt)

	// --- Turn 1: trigger a task transfer ---
	handle, err := supervisor.Runtime().CreateSession(ctx,
		session.New(session.WithNonInteractive(true)), runtime.SessionBinding{})
	require.NoError(t, err)
	sess, err := runTestTurn(ctx, handle, "What's the weather in Paris? Delegate to the weather agent.")
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	require.NotEmpty(t, response, "first turn should produce a response")

	// The store is independently owned and stays open across runtime shutdown.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	require.NoError(t, supervisor.Shutdown(cleanupCtx))

	// --- Reload the session from the store ---
	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded)

	// --- Turn 2: follow-up with a fresh owner of the durable snapshot ---
	restarted, err := runtime.New(ctx, team, runtime.WithSessionStore(store))
	require.NoError(t, err)
	restartedSupervisor := ownTestRuntime(t, restarted)
	reloadedHandle, err := restartedSupervisor.Runtime().CreateSession(ctx, reloaded, runtime.SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, handle.ID(), reloadedHandle.ID())
	assert.NotSame(t, handle, reloadedHandle)
	completed, err := runTestTurn(ctx, reloadedHandle, "Can you summarize what you found?")
	require.NoError(t, err, "follow-up on reloaded session should not fail; "+
		"orphan sub-agent messages in the persisted parent session would cause "+
		"model API errors due to corrupted message sequence")

	response2 := completed.GetLastAssistantMessageContent()
	assert.NotEmpty(t, response2, "second turn should produce a response")
}

func TestRuntime_Mistral_Basic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	agentSource, err := sources.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	_, runConfig := startRecordingAIProxy(t)
	team, err := teamloader.Load(ctx, agentSource, runConfig, append(loaderdefaults.Opts(), teamloader.WithModelOverrides([]string{"mistral/mistral-small"}))...)
	require.NoError(t, err)

	rt, err := runtime.New(t.Context(), team)
	require.NoError(t, err)

	supervisor := ownTestRuntime(t, rt)
	handle, err := supervisor.Runtime().CreateSession(ctx,
		session.New(session.WithNonInteractive(true)), runtime.SessionBinding{})
	require.NoError(t, err)
	sess, err := runTestTurn(ctx, handle, "What's 2+2?")
	require.NoError(t, err)

	response := sess.GetLastAssistantMessageContent()
	assert.Equal(t, "The sum of 2 + 2 is 4.", response)
	// Title generation is now handled by pkg/app or pkg/server, not the runtime
}

func ownTestRuntime(t *testing.T, rt *runtime.LocalRuntime) runtime.SessionRuntimeSupervisor {
	t.Helper()
	supervisor := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		require.NoError(t, supervisor.Shutdown(ctx))
	})
	return supervisor
}

func runTestTurn(ctx context.Context, handle runtime.SessionHandle, prompt string) (*session.Session, error) {
	ownedTurn, err := turn.Start(ctx, handle, runtime.TurnInput{Content: prompt})
	if err != nil {
		return nil, err
	}
	termination := ownedTurn.Consume(ctx, func(_ context.Context, envelope runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
		if event, ok := envelope.Event.(*runtime.ErrorEvent); ok {
			return runtimeclient.TurnTerminate, errors.New(event.Error)
		}
		return runtimeclient.TurnContinue, nil
	})
	if termination.Err != nil {
		return nil, termination.Err
	}
	return handle.Snapshot(ctx)
}
