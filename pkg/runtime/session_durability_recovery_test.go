package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestPromotedTurnRestoreRequiresTerminalEvidence(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "crash.db"))
	require.NoError(t, err)
	defer store.Close()
	s := session.New(session.WithID("interrupted"), session.WithAgentName("root"))
	s.SetAttribute(SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), s))
	input := session.UserMessage("task that never reached a provider")
	input.TurnID, input.Accepted, input.Pending = "lost-turn", true, true
	_, err = store.AddMessage(t.Context(), s.ID, input)
	require.NoError(t, err)
	// Exact committed operation in prepareStart, immediately before any provider runs.
	require.NoError(t, store.PromotePendingUserMessage(t.Context(), s.ID, input.TurnID))
	loaded, err := store.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	r := newPersistedSessionRuntime(t, store)
	h, err := r.CreateSession(t.Context(), loaded, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	st, err := h.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, SessionStateSettled, st.State)
	require.Zero(t, st.Pending)
	require.Equal(t, 1, st.InterruptedTurns)
	require.ErrorIs(t, h.AwaitTurn(t.Context(), input.TurnID), &SessionError{Kind: SessionErrorInterrupted})
	require.Empty(t, loaded.GetLastAssistantMessageContent())
}

type recoveryMetadataStore struct {
	session.Store

	failNext bool
}

func (s *recoveryMetadataStore) UpdateSession(ctx context.Context, value *session.Session) error {
	if s.failNext {
		s.failNext = false
		return &session.TemporaryError{Err: errors.New("one transient metadata failure")}
	}
	return s.Store.UpdateSession(ctx, value)
}

func TestMetadataRetryPreservesAcknowledgedPolicyRevocation(t *testing.T) {
	base, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "policy.db"))
	require.NoError(t, err)
	defer base.Close()
	wrapper := &recoveryMetadataStore{Store: base}
	r := newPersistedSessionRuntime(t, wrapper)
	s := session.New(session.WithID("policy"), session.WithAgentName("root"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	h, err := r.CreateSession(t.Context(), s, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	p := newPersistenceObserver(wrapper)
	p.owner = func(id string) *sessionDriver { d, _ := r.sessionDrivers.Lookup(id); return d }
	wrapper.failNext = true
	p.OnRunStart(t.Context(), h.(*sessionHandle).driver.session())
	require.Error(t, p.pendingError(s.ID))
	strict := session.SafetyPolicyStrict
	_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyPolicy: &strict})
	require.NoError(t, err)
	before, err := base.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Equal(t, strict, before.GetSafetyPolicy())
	require.NoError(t, p.flushContext(t.Context(), s.ID))
	after, err := base.GetSession(t.Context(), s.ID)
	require.NoError(t, err)
	require.Equal(t, strict, after.GetSafetyPolicy())
	require.Equal(t, strict, h.(*sessionHandle).driver.session().GetSafetyPolicy())
}

func TestDurableTurnTerminalEvidenceSurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		name, runErr string
		canceled     bool
		outcome      TurnOutcome
	}{
		{name: "completed", outcome: TurnCompleted},
		{name: "failed", runErr: "provider failed", outcome: TurnFailed},
		{name: "canceled", canceled: true, outcome: TurnCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "turn.db"))
			require.NoError(t, err)
			defer base.Close()
			store := &recoveryMetadataStore{Store: base}
			r := newPersistedSessionRuntime(t, store)
			sess := session.New(session.WithID("terminal"), session.WithAgentName("root"))
			handle, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			r.sessionDrivers.mu.Lock()
			r.sessionDrivers.closed = true
			r.sessionDrivers.mu.Unlock()
			d := handle.(*sessionHandle).driver
			msg := QueuedMessage{RequestID: "turn", Content: "work", InputOrigin: session.InputOriginUser}
			_, err = d.admitInput(t.Context(), msg, SessionOperationPost, true, false)
			require.NoError(t, err)
			require.NoError(t, d.promoteInput(t.Context(), msg.RequestID, func(QueuedMessage) { d.pending = nil }))
			d.mu.Lock()
			d.activeRequestID, d.generation, d.phase = "turn", 1, sessionRunning
			if tc.canceled {
				d.phase = sessionCancelling
			}
			d.mu.Unlock()
			store.failNext = true
			d.finishRun(1, tc.runErr)
			require.Error(t, d.completionErr)
			loaded, err := base.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Empty(t, loaded.TurnOutcomesSnapshot())
			require.Empty(t, canonicalSettlements(canonicalReplay(d)))
			d.finishRun(1, tc.runErr)
			require.NoError(t, d.completionErr)
			require.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
			loaded, err = base.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, string(tc.outcome), loaded.TurnOutcomesSnapshot()["turn"])
			restored := newSessionDriver(r, loaded)
			restoredHandle := &sessionHandle{driver: restored, sessionID: sess.ID}
			require.NoError(t, restoredHandle.AwaitTurn(t.Context(), "turn"))
			require.Empty(t, restored.pending)
			require.Zero(t, restored.Status().InterruptedTurns)
		})
	}
}

func TestMetadataRetryPreservesAcknowledgedPermissions(t *testing.T) {
	base, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "permissions.db"))
	require.NoError(t, err)
	defer base.Close()
	store := &recoveryMetadataStore{Store: base}
	r := newPersistedSessionRuntime(t, store)
	handle, err := r.CreateSession(t.Context(), session.New(session.WithID("permissions")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observer := newPersistenceObserver(store)
	observer.owner = func(id string) *sessionDriver { d, _ := r.sessionDrivers.Lookup(id); return d }
	store.failNext = true
	observer.OnRunStart(t.Context(), handle.(*sessionHandle).driver.session())
	permissions := &session.PermissionsConfig{Deny: []string{"shell"}}
	_, err = handle.Edit(t.Context(), SessionEdit{Kind: SessionEditPermissions, Permissions: permissions})
	require.NoError(t, err)
	require.NoError(t, observer.flushContext(t.Context(), handle.ID()))
	loaded, err := base.GetSession(t.Context(), handle.ID())
	require.NoError(t, err)
	require.Equal(t, permissions, loaded.ClonePermissions())
}

func TestEvictedTurnTerminalEvidenceRestoresUncertainty(t *testing.T) {
	sess := session.New(session.WithID("retained"))
	message := session.UserMessage("old work")
	message.TurnID, message.Accepted = "old", true
	sess.AddMessage(message)
	sess.SetTurnOutcome("old", string(TurnCompleted))
	for i := range session.MaxRetainedTurnOutcomes {
		sess.SetTurnOutcome(strconv.Itoa(i), string(TurnCompleted))
	}
	require.Len(t, sess.TurnOutcomesSnapshot(), session.MaxRetainedTurnOutcomes)
	require.Empty(t, sess.TurnOutcome("old"))
	driver := newSessionDriver(newDriverTestRuntime(t), sess)
	require.Equal(t, 1, driver.Status().InterruptedTurns)
	handle := &sessionHandle{driver: driver, sessionID: sess.ID}
	require.ErrorIs(t, handle.AwaitTurn(t.Context(), "old"), &SessionError{Kind: SessionErrorInterrupted})
}

func TestPendingTurnRestoreIsNotInterrupted(t *testing.T) {
	sess := session.New(session.WithID("pending"))
	message := session.UserMessage("safe queued work")
	message.TurnID, message.Accepted, message.Pending = "queued", true, true
	sess.AddMessage(message)
	driver := newSessionDriver(newDriverTestRuntime(t), sess)
	status := driver.Status()
	require.Equal(t, SessionStateQueued, status.State)
	require.Equal(t, 1, status.Pending)
	require.Zero(t, status.InterruptedTurns)
}

type recoveryTitleStore struct {
	session.Store

	failure error
}

func (s *recoveryTitleStore) UpdateSessionTitle(ctx context.Context, id, title string) error {
	if s.failure != nil {
		return s.failure
	}
	return s.Store.UpdateSessionTitle(ctx, id, title)
}

func TestLegacyTitleUpdateUsesAuthoritativeOwnerAndPublishesAfterCommit(t *testing.T) {
	for _, resident := range []bool{false, true} {
		t.Run(strconv.FormatBool(resident), func(t *testing.T) {
			base, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "title.db"))
			require.NoError(t, err)
			defer base.Close()
			store := &recoveryTitleStore{Store: base}
			r := newPersistedSessionRuntime(t, store)
			sess := session.New(session.WithID("title"), session.WithAgentName("root"), session.WithSafetyPolicy(session.SafetyPolicyStrict))
			sess.SetTitle("before")
			var driver *sessionDriver
			if resident {
				handle, err := r.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
				require.NoError(t, err)
				driver = handle.(*sessionHandle).driver
				sess = driver.session()
			} else {
				require.NoError(t, base.AddSession(t.Context(), sess))
			}
			stale := sess.Clone()
			stale.SetSafetyPolicy(session.SafetyPolicyAutonomous)
			failure := errors.New("title write rejected")
			store.failure = failure
			require.ErrorIs(t, r.UpdateSessionTitle(t.Context(), stale, "failed"), failure)
			require.Equal(t, "before", sess.TitleSnapshot())
			require.Equal(t, "before", stale.TitleSnapshot())
			if resident {
				require.Empty(t, canonicalReplay(driver))
			}
			store.failure = nil
			require.NoError(t, r.UpdateSessionTitle(t.Context(), stale, "after"))
			persisted, err := base.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Equal(t, "after", persisted.TitleSnapshot())
			require.Equal(t, session.SafetyPolicyStrict, persisted.GetSafetyPolicy())
			if resident {
				canonical, err := driver.ownerSnapshot(t.Context())
				require.NoError(t, err)
				require.Equal(t, "after", canonical.TitleSnapshot())
				require.Equal(t, session.SafetyPolicyStrict, canonical.GetSafetyPolicy())
				require.Equal(t, "before", sess.TitleSnapshot(), "detached snapshot is not live edit authority")
				require.Equal(t, "before", stale.TitleSnapshot())
				require.Len(t, canonicalReplay(driver), 1)
			} else {
				require.Equal(t, "after", stale.TitleSnapshot())
			}
		})
	}
}
