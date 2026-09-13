package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestViewCancellationAndCancelLeaveHandleResumable(t *testing.T) {
	release := make(chan struct{})
	provider := &mockProvider{id: "test/blocking", stream: &blockingMockStream{release: release}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(provider)),
	)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })

	sess := session.New(session.WithID("persisted-like"), session.WithAgentName("root"))
	for i := range 30 {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("persisted message %d", i)))
	}
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obsCtx, cancelView := context.WithCancel(t.Context())
	obs, err := handle.Observe(obsCtx, ObserveOptions{})
	require.NoError(t, err)
	_, err = handle.Submit(t.Context(), TurnInput{Content: "first after open"})
	require.NoError(t, err)
	for envelope := range obs.Events {
		if _, started := envelope.Event.(*StreamStartedEvent); started {
			break
		}
	}
	cancelView()
	for range obs.Events {
	}
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateRunning, status.State, "view cancellation only detaches observation")

	wake := handle.(interface {
		Cancel(ctx context.Context, turnID string) (CancelResult, error)
	})
	result, err := wake.Cancel(t.Context(), func() string { st, _ := handle.Status(t.Context()); return st.TurnID }())
	require.NoError(t, err)
	assert.Equal(t, CancelAccepted, result.Outcome)
	require.Eventually(t, func() bool {
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == SessionStateSettled
	}, 5*time.Second, 10*time.Millisecond)

	_, err = handle.Submit(t.Context(), TurnInput{Content: "second turn"})
	require.NoError(t, err)
	close(release)
}

type transientPromotionStore struct {
	session.Store

	failures atomic.Int64
}

func (s *transientPromotionStore) PromotePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	if s.failures.Add(-1) >= 0 {
		return &session.TemporaryError{Err: errors.New("transient promotion")}
	}
	return s.Store.PromotePendingUserMessage(ctx, sessionID, turnID)
}

type restartRecordingProvider struct {
	*multiRunProvider

	mu       sync.Mutex
	requests [][]chat.Message
}

func (p *restartRecordingProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, availableTools []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]chat.Message(nil), messages...))
	p.mu.Unlock()
	return p.multiRunProvider.CreateChatCompletionStream(ctx, messages, availableTools)
}

func (p *restartRecordingProvider) userRequests() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([][]string, len(p.requests))
	for i, messages := range p.requests {
		for _, message := range messages {
			if message.Role == chat.MessageRoleUser {
				requests[i] = append(requests[i], message.Content)
			}
		}
	}
	return requests
}

func TestPublicSQLiteSubmitBehindTransientRestoredHeadRecoversOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "public-retry.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	persisted := session.New(session.WithID("public-retry"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), persisted))
	a := session.UserMessage("A")
	a.Pending, a.Accepted, a.TurnID = true, true, "turn-a"
	_, err = store.AddMessage(t.Context(), persisted.ID, a)
	require.NoError(t, err)
	loaded, err := store.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	wrapped := &transientPromotionStore{Store: store}
	releaseA := make(chan struct{})
	var builds atomic.Int64
	var mu sync.Mutex
	provider := &restartRecordingProvider{multiRunProvider: &multiRunProvider{mockProvider: &mockProvider{id: "test/public-retry"}, build: func() chat.MessageStream {
		if builds.Add(1) == 1 {
			return &blockingMockStream{release: releaseA}
		}
		return newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()
	}}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))), WithSessionStore(wrapped))
	require.NoError(t, err)
	h, err := rt.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{Buffer: 128})
	require.NoError(t, err)
	defer obs.Cancel()
	failures := map[string]int{}
	for _, envelope := range obs.Replay {
		if _, ok := envelope.Event.(*ErrorEvent); ok {
			failures[envelope.TurnID]++
		}
	}
	consumeDone := make(chan struct{})
	go func() {
		defer close(consumeDone)
		for envelope := range obs.Events {
			if _, ok := envelope.Event.(*ErrorEvent); ok {
				mu.Lock()
				failures[envelope.TurnID]++
				mu.Unlock()
			}
		}
	}()
	require.Eventually(t, func() bool { return builds.Load() == 1 }, time.Second, time.Millisecond)
	wrapped.failures.Store(1)
	b, err := h.Submit(t.Context(), TurnInput{Content: "B"})
	require.NoError(t, err)
	close(releaseA)
	require.NotEmpty(t, b.TurnID)
	require.Eventually(t, func() bool {
		status, _ := h.Status(t.Context())
		return status.State == SessionStateSettled && status.Pending == 0
	}, 5*time.Second, time.Millisecond)
	obs.Cancel()
	<-consumeDone
	mu.Lock()
	assert.Equal(t, 1, failures[b.TurnID])
	assert.Zero(t, failures["turn-a"])
	mu.Unlock()
	got := provider.userRequests()
	require.Len(t, got, 2)
	assert.Equal(t, []string{"A"}, got[0])
	assert.Equal(t, []string{"A", "B"}, got[1])
	reloaded, err := store.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	counts := map[string]int{}
	for _, message := range reloaded.GetAllMessages() {
		if message.Message.Role == chat.MessageRoleUser {
			counts[message.Message.Content]++
		}
		assert.False(t, message.Pending)
	}
	status, err := h.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateSettled, status.State)
	assert.Empty(t, status.TurnID)
	assert.Zero(t, status.Pending)
	assert.Equal(t, "ok", reloaded.GetLastAssistantMessageContent())
	assert.Equal(t, 1, counts["A"])
	assert.Equal(t, 1, counts["B"])

	obs.Cancel()
	require.NoError(t, rt.Close())
	require.NoError(t, store.Close())
	for reopen := range 2 {
		reopened, openErr := sqlitestore.New(t.Context(), dbPath)
		require.NoError(t, openErr)
		restored, loadErr := reopened.GetSession(t.Context(), persisted.ID)
		require.NoError(t, loadErr)
		var replayCalls atomic.Int64
		p := &multiRunProvider{mockProvider: &mockProvider{id: fmt.Sprintf("test/reopen-%d", reopen)}, build: func() chat.MessageStream {
			replayCalls.Add(1)
			return newStreamBuilder().AddContent("unexpected").Build()
		}}
		r, runtimeErr := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(p)))), WithSessionStore(reopened))
		require.NoError(t, runtimeErr)
		_, createErr := r.CreateSession(t.Context(), restored, SessionBinding{AgentName: "root"})
		require.NoError(t, createErr)
		time.Sleep(25 * time.Millisecond) //nolint:forbidigo // deliberate observation window proving no stale run starts
		assert.Zero(t, replayCalls.Load(), "cold reopen %d must not replay completed A/B", reopen+1)
		require.NoError(t, r.Close())
		require.NoError(t, reopened.Close())
	}
}

func TestPersistedSessionFirstSubmitAfterFreshRuntime(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	persisted := session.New(session.WithID("persisted-root"), session.WithAgentName("root"))
	persisted.AddMessage(session.UserMessage("prior question"))
	persisted.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "prior answer"}})
	require.NoError(t, store.AddSession(t.Context(), persisted))

	loaded, err := store.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	rt := newPersistedSessionRuntime(t, store)
	handle, err := rt.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()

	submission, err := handle.Submit(t.Context(), TurnInput{Content: "first after restart"})
	require.NoError(t, err)
	for envelope := range obs.Events {
		if envelope.TurnID == submission.TurnID {
			if _, stopped := envelope.Event.(*StreamStoppedEvent); stopped {
				break
			}
		}
	}
	assert.Equal(t, "ok", sessionHandleSnapshot(t, handle).GetLastAssistantMessageContent())
}

func TestPersistedAcceptedPendingInputResumesOnceAcrossTwoRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	storeA, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	persisted := session.New(session.WithID("persisted-pending-twice"), session.WithAgentName("root"))
	require.NoError(t, storeA.AddSession(t.Context(), persisted))
	pending := session.UserMessage("resume exactly once")
	pending.Pending, pending.Accepted, pending.TurnID = true, true, "durable-turn"
	_, err = storeA.AddMessage(t.Context(), persisted.ID, pending)
	require.NoError(t, err)
	require.NoError(t, storeA.Close())

	storeB, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	loaded, err := storeB.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	provider := &restartRecordingProvider{multiRunProvider: &multiRunProvider{mockProvider: &mockProvider{id: "test/restart"}, build: func() chat.MessageStream {
		return newStreamBuilder().AddContent("resumed answer").AddStopWithUsage(1, 1).Build()
	}}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider)))), WithSessionStore(storeB))
	require.NoError(t, err)
	handle, err := rt.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, _ := handle.Status(t.Context())
		return status.State == SessionStateSettled && len(provider.userRequests()) == 1
	}, 5*time.Second, time.Millisecond)
	calls := provider.userRequests()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"resume exactly once"}, calls[0])
	require.NoError(t, rt.shutdownSessions(t.Context()))
	require.NoError(t, storeB.Close())

	storeC, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer storeC.Close()
	reloaded, err := storeC.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	for _, item := range reloaded.MessagesSnapshot() {
		if item.Message != nil {
			assert.False(t, item.Message.Pending, "durable pending flag clears before provider consumption")
		}
	}
	rt2 := newPersistedSessionRuntime(t, storeC)
	reopenedHandle, err := rt2.CreateSession(t.Context(), reloaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond) //nolint:forbidigo // deliberate observation window proving stale generation is fenced
	assert.Equal(t, "resumed answer", sessionHandleSnapshot(t, reopenedHandle).GetLastAssistantMessageContent(), "second reload must not replay the promoted input")
}

func TestPersistedAcceptedPendingInputAutoResumesOnFreshRuntime(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	persisted := session.New(session.WithID("persisted-pending"), session.WithAgentName("root"))
	pending := session.UserMessage("resume me")
	pending.Pending = true
	pending.Accepted = true
	pending.TurnID = "durable-turn"
	require.NoError(t, store.AddSession(t.Context(), persisted))
	_, err = store.AddMessage(t.Context(), persisted.ID, pending)
	require.NoError(t, err)

	loaded, err := store.GetSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	rt := newPersistedSessionRuntime(t, store)
	handle, err := rt.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.State == SessionStateSettled && sessionHandleSnapshot(t, handle).GetLastAssistantMessageContent() == "ok"
	}, 5*time.Second, time.Millisecond, "durably accepted work resumes without client input")
	messages := sessionHandleSnapshot(t, handle).GetAllMessages()
	require.NotEmpty(t, messages)
	var restored session.Message
	var found bool
	for _, message := range messages {
		if message.TurnID == "durable-turn" {
			restored = message
			found = true
			break
		}
	}
	require.True(t, found)
	assert.False(t, restored.Pending)
}

func TestReleasedSessionCanReopenWhileStaleHandleStaysStopped(t *testing.T) {
	rt, sess := newSessionFixture(t)
	stale, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, stale.Release(t.Context()))

	reopenedSession := sess.Clone()
	current, err := rt.CreateSession(t.Context(), reopenedSession, SessionBinding{})
	require.NoError(t, err)
	require.NotSame(t, stale, current)
	_, err = stale.Submit(t.Context(), TurnInput{Content: "stale"})
	require.ErrorIs(t, err, ErrSessionStopped)
	_, err = current.Submit(t.Context(), TurnInput{Content: "new generation"})
	require.NoError(t, err)
}

func TestConcurrentCreateSessionPublishesBindingOnce(t *testing.T) {
	rt, sess := newSessionFixture(t)
	const callers = 32
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			_, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	handle, err := rt.SessionByID(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", handle.AgentName())
	assert.Equal(t, "root", sessionHandleSnapshot(t, handle).AgentName)
	assert.Empty(t, sess.AgentName, "binding must not mutate the caller-owned template")
}

func TestConcurrentGenerationLifecyclePreservesDeleteFinality(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, handle.Release(t.Context()))

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, _ = rt.SessionByID(sess.ID)
			_, _ = rt.CreateSession(t.Context(), sess, SessionBinding{})
		})
	}
	wg.Wait()
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))

	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.ErrorIs(t, err, ErrSessionStopped)
}

func TestStaleHandleReleaseDoesNotRemoveNewGeneration(t *testing.T) {
	rt, sess := newSessionFixture(t)
	staleSession, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	stale := staleSession.(*sessionHandle)
	require.NoError(t, stale.Release(t.Context()))

	currentSession, err := rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err)
	current := currentSession.(*sessionHandle)
	require.NoError(t, stale.Release(t.Context()))

	resolved, err := rt.SessionByID(sess.ID)
	require.NoError(t, err)
	assert.Same(t, current.driver, resolved.(*sessionHandle).driver)
	_, err = current.Submit(t.Context(), TurnInput{Content: "current survives stale cleanup"})
	require.NoError(t, err)
}

func TestCloseSessionsCannotBeHealedByGenerationReopen(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.shutdownSessions(t.Context()))

	_, err = handle.Submit(t.Context(), TurnInput{Content: "stale"})
	require.ErrorIs(t, err, ErrSessionStopped)
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorClosed, sessionErr.Kind)
}

func newPersistedSessionRuntime(t *testing.T, store session.Store) *LocalRuntime {
	t.Helper()
	provider := &mockProvider{id: "test/persisted", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(provider)),
	)), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.shutdownSessions(context.WithoutCancel(t.Context()))) })
	return rt
}

func TestLocalSessionCatalogRejectsLegacyUnboundSessions(t *testing.T) {
	store := session.NewInMemorySessionStore()
	legacy := session.New(session.WithID("legacy"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), legacy))
	rt := newPersistedSessionRuntime(t, store)
	catalog := NewSessionRuntimeSupervisor(rt).Runtime().(SessionCatalog)

	rows, err := catalog.ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Loadable)
	assert.Empty(t, rows[0].AgentName)

	loader := NewSessionRuntimeSupervisor(rt).Runtime().(SessionLoader)
	_, _, err = loader.LoadSession(t.Context(), legacy.ID)
	require.Error(t, err)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorUnsupported, sessionErr.Kind)
	assert.Equal(t, SessionOperationAttach, sessionErr.Operation)
	_, found := rt.sessionDrivers.Lookup(legacy.ID)
	assert.False(t, found)
}

func TestLocalSessionCatalogLoadsPersistedSessionBinding(t *testing.T) {
	store := session.NewInMemorySessionStore()
	persisted := session.New(session.WithID("bound"))
	persisted.SetAttribute(SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), persisted))
	rt := newPersistedSessionRuntime(t, store)
	view := NewSessionRuntimeSupervisor(rt).Runtime()

	rows, err := view.(SessionCatalog).ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Loadable)
	assert.Equal(t, "root", rows[0].AgentName)

	handle, loaded, err := view.(SessionLoader).LoadSession(t.Context(), persisted.ID)
	require.NoError(t, err)
	assert.Equal(t, "root", handle.AgentName())
	assert.Equal(t, "root", loaded.AgentName)
}

func TestExistingLegacySessionRemainsUnstampedAfterSessionPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	legacy := session.New(session.WithID("legacy"), session.WithWorkingDir(t.TempDir()))
	require.NoError(t, store.AddSession(t.Context(), legacy))
	loaded, err := store.GetSession(t.Context(), legacy.ID)
	require.NoError(t, err)
	assert.Empty(t, loaded.AttributesSnapshot()[SessionAgentAttribute])

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/legacy"})),
	)), WithSessionStore(store))
	require.NoError(t, err)
	handle, err := rt.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.NoError(t, handle.UpdateTitle(t.Context(), "legacy updated"))
	require.NoError(t, rt.shutdownSessions(t.Context()))
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer reopened.Close()
	persisted, err := reopened.GetSession(t.Context(), legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, "legacy updated", persisted.Title)
	assert.Empty(t, persisted.AttributesSnapshot()[SessionAgentAttribute])
}

func TestFreshSessionHandlePersistsAcrossSQLiteReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/persist"})),
	)), WithSessionStore(store))
	require.NoError(t, err)

	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	_, err = rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.NoError(t, rt.shutdownSessions(t.Context()))
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer reopened.Close()
	loaded, err := reopened.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.WorkingDir, loaded.WorkingDir)
	assert.Equal(t, "root", loaded.AttributesSnapshot()[SessionAgentAttribute])

	summaries, err := reopened.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, sess.ID, summaries[0].ID)

	rt2, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/reopen"})),
	)), WithSessionStore(reopened))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt2.shutdownSessions(context.WithoutCancel(t.Context()))) })
	handle, err := rt2.CreateSession(t.Context(), loaded, SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, "root", handle.AgentName())
}
