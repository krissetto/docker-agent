package tui

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

type lifecycleSessions struct {
	mu        sync.Mutex
	handles   map[string]*lifecycleHandle
	deletes   []string
	deleteErr error
}

type lifecycleHandle struct {
	runtime.UnsupportedSessionHandle

	id       string
	submits  atomic.Int32
	releases atomic.Int32
	cancels  atomic.Int32
}

func newLifecycleSessions() *lifecycleSessions {
	return &lifecycleSessions{handles: map[string]*lifecycleHandle{}}
}

func (r *lifecycleSessions) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := &lifecycleHandle{id: sess.ID}
	r.handles[sess.ID] = h
	return h, nil
}

func (r *lifecycleSessions) SessionByID(id string) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.handles[id]; h != nil {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}

func (r *lifecycleSessions) DeleteSession(_ context.Context, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, sessionID)
	return r.deleteErr
}
func (h *lifecycleHandle) ID() string      { return h.id }
func (*lifecycleHandle) AgentName() string { return "root" }
func (h *lifecycleHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: "root"}
}

func (h *lifecycleHandle) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	h.submits.Add(1)
	return runtime.Submission{SessionID: h.id, TurnID: "accepted"}, nil
}

func (h *lifecycleHandle) Retry(ctx context.Context) (runtime.Submission, error) {
	return h.Submit(ctx, runtime.TurnInput{Retry: true})
}

func (h *lifecycleHandle) Steer(ctx context.Context, in runtime.TurnInput) (runtime.Submission, error) {
	return h.Submit(ctx, in)
}

func (h *lifecycleHandle) Observe(ctx context.Context, _ runtime.ObserveOptions) (runtime.Observation, error) {
	out := make(chan runtime.SessionEvent)
	obsCtx, cancel := context.WithCancel(ctx)
	go func() { <-obsCtx.Done(); close(out) }()
	return runtime.Observation{Events: out, Cancel: func() { h.cancels.Add(1); cancel() }}, nil
}

func (h *lifecycleHandle) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: h.id}, nil
}
func (*lifecycleHandle) Respond(context.Context, runtime.InteractionResponse) error { return nil }
func (*lifecycleHandle) UpdateTitle(context.Context, string) error                  { return nil }
func (h *lifecycleHandle) Cancel(context.Context, string) (runtime.CancelResult, error) {
	return runtime.CancelResult{SessionID: h.id, Outcome: runtime.CancelAccepted}, nil
}
func (h *lifecycleHandle) Release(context.Context) error { h.releases.Add(1); return nil }

func TestColdRestoreWorkingDirReplacementRetainsSharedRuntime(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(dir)
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })

	store := session.NewInMemorySessionStore()
	persisted := session.New(session.WithID("persisted"), session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	for i := range 30 {
		persisted.AddMessage(session.UserMessage(string(rune('a' + i%26))))
	}
	require.NoError(t, store.AddSession(t.Context(), persisted))

	sessions := newLifecycleSessions()
	services := storeRuntime{store: store}
	blank := session.New(session.WithID("blank"), session.WithAgentName("root"), session.WithWorkingDir(dir))
	initial := app.New(t.Context(), sessions, blank, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	var ownerCleanup atomic.Int32
	spawner := func(ctx context.Context, workingDir string) (SpawnedSession, error) {
		transient := session.New(session.WithAgentName("root"), session.WithWorkingDir(workingDir))
		return SpawnedSession{
			App:     app.New(ctx, sessions, transient, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services)),
			Session: transient, Ownership: RuntimeBorrowed,
		}, nil
	}
	model := New(t.Context(), spawner, initial, dir, func() { ownerCleanup.Add(1) })
	m := model.(*appModel)
	t.Cleanup(m.cleanupManagedResources)

	// Drive the model through a real program: session loading now finishes
	// asynchronously (git-branch watching, transcript restore), so a
	// synchronous cmd-feeding loop would block on the watcher.
	driver := tuitest.New(t, model, 120, 40)
	driver.Send(messages.LoadSessionMsg{SessionID: persisted.ID})
	driver.WaitFor(tuitest.Contains("a"))
	require.Eventually(t, func() bool { return m.application.Session().ID == persisted.ID }, 5*time.Second, 10*time.Millisecond)
	assert.Zero(t, ownerCleanup.Load(), "borrowed replacement must not close the shared runtime")

	// Replacement cancels only the blank/transient observations. The restored
	// session accepts its first and subsequent turns, including after another
	// view observation is attached and cancelled.
	_, err := m.application.FollowUpMessage(t.Context(), "first after restore", nil)
	require.NoError(t, err)
	restored := sessions.handles[persisted.ID]
	require.NotNil(t, restored)
	obsCtx, cancel := context.WithCancel(t.Context())
	obs, err := restored.Observe(obsCtx, runtime.ObserveOptions{})
	require.NoError(t, err)
	cancel()
	obs.Cancel()
	_, err = m.application.FollowUpMessage(t.Context(), "second after view cancel", nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), restored.submits.Load())

	m.cleanupManagedResources()
	assert.Equal(t, int32(1), ownerCleanup.Load())
}

type deleteTrackingStore struct {
	session.Store

	deletes   []string
	deleteErr error
}

func (s *deleteTrackingStore) DeleteSession(_ context.Context, id string) error {
	s.deletes = append(s.deletes, id)
	return s.deleteErr
}

func TestHandleDeleteSessionUsesSessionRuntimeWithoutStoreDoubleDelete(t *testing.T) {
	t.Parallel()

	store := &deleteTrackingStore{Store: session.NewInMemorySessionStore()}
	sessions := newLifecycleSessions()
	sess := session.New(session.WithID("session-owned-delete"), session.WithAgentName("root"))
	a := app.New(t.Context(), sessions, sess, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	m := &appModel{application: a, ctx: func() context.Context { return t.Context() }}

	_, cmd := m.handleDeleteSession(sess.ID)

	require.NotNil(t, cmd)
	assert.Equal(t, []string{sess.ID}, sessions.deletes)
	assert.Empty(t, store.deletes, "session-owned deletion already performs canonical store cleanup")
}

func TestHandleDeleteSessionFallsBackToStoreWithoutSessionRuntime(t *testing.T) {
	t.Parallel()

	store := &deleteTrackingStore{Store: session.NewInMemorySessionStore()}
	sess := session.New(session.WithID("legacy-delete"), session.WithAgentName("root"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	m := &appModel{application: a, ctx: func() context.Context { return t.Context() }}

	_, cmd := m.handleDeleteSession(sess.ID)

	require.NotNil(t, cmd)
	assert.Equal(t, []string{sess.ID}, store.deletes)
}

func TestHandleDeleteSessionHandleErrorDoesNotFallBackToStore(t *testing.T) {
	t.Parallel()

	deleteErr := errors.New("session cleanup failed")
	store := &deleteTrackingStore{Store: session.NewInMemorySessionStore()}
	sessions := newLifecycleSessions()
	sessions.deleteErr = deleteErr
	sess := session.New(session.WithID("session-delete-error"), session.WithAgentName("root"))
	a := app.New(t.Context(), sessions, sess, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	m := &appModel{application: a, ctx: func() context.Context { return t.Context() }}

	_, cmd := m.handleDeleteSession(sess.ID)

	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	assert.Equal(t, notification.ShowMsg{Text: "Failed to delete session: " + deleteErr.Error(), Type: notification.TypeError}, msgs[0])
	assert.Equal(t, []string{sess.ID}, sessions.deletes)
	assert.Empty(t, store.deletes, "a session error must not trigger a second non-canonical delete")
}

func TestHandleDeleteSessionStoreFallbackErrorNotifies(t *testing.T) {
	t.Parallel()

	deleteErr := errors.New("store unavailable")
	store := &deleteTrackingStore{Store: session.NewInMemorySessionStore(), deleteErr: deleteErr}
	sess := session.New(session.WithID("store-delete-error"), session.WithAgentName("root"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	m := &appModel{application: a, ctx: func() context.Context { return t.Context() }}

	_, cmd := m.handleDeleteSession(sess.ID)

	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	assert.Equal(t, notification.ShowMsg{Text: "Failed to delete session: " + deleteErr.Error(), Type: notification.TypeError}, msgs[0])
	assert.Equal(t, []string{sess.ID}, store.deletes)
}

// TestClosingInitialTabKeepsSharedRuntimeAliveForBorrowedTabs pins the
// ownership rule of the shared local runtime: tabs that borrow it must
// survive the initial tab closing. The runtime belongs to the run (it is
// shut down when the TUI exits), not to whichever tab happened to be first.
func TestClosingInitialTabKeepsSharedRuntimeAliveForBorrowedTabs(t *testing.T) {
	dir := t.TempDir()
	paths.SetDataDir(dir)
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetDataDir(""); paths.SetConfigDir("") })

	sessions := newLifecycleSessions()
	services := storeRuntime{store: session.NewInMemorySessionStore()}
	initialSess := session.New(session.WithID("initial"), session.WithAgentName("root"), session.WithWorkingDir(dir))
	initial := app.New(t.Context(), sessions, initialSess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	var ownerCleanup atomic.Int32
	spawner := func(ctx context.Context, workingDir string) (SpawnedSession, error) {
		sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(workingDir))
		return SpawnedSession{
			App:       app.New(ctx, sessions, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services)),
			Session:   sess,
			Ownership: RuntimeBorrowed,
		}, nil
	}
	model := New(t.Context(), spawner, initial, dir, func() { ownerCleanup.Add(1) })
	m := model.(*appModel)
	t.Cleanup(m.cleanupManagedResources)

	_, _ = m.handleSpawnSession(dir)
	require.Equal(t, 2, m.supervisor.Count())

	_, _ = m.handleCloseTab("initial")
	require.Equal(t, 1, m.supervisor.Count())
	// A tab-close cleanup would run on its own goroutine; the run-scoped
	// cleanup below is synchronous, so observing exactly one call after it
	// proves the tab close contributed none.
	m.cleanupManagedResources()
	assert.Equal(t, int32(1), ownerCleanup.Load(), "closing the initial tab must not shut down the runtime other tabs borrow; only the TUI exit does, once")
}
