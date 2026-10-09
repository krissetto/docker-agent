package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func inputBarrier(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("input barrier timed out")
	}
}

func inputResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("input result timed out")
		return nil
	}
}

type receiptBarrierStore struct {
	session.Store
	session.CoordinationStore

	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	uncertain bool
}

func (s *receiptBarrierStore) AppendItem(ctx context.Context, id, key string, item session.Item) (int64, error) {
	row, err := s.Store.(session.ItemAppender).AppendItem(ctx, id, key, item)
	if err == nil && item.Message != nil && item.Message.Message.Content == "late input" {
		first := false
		s.once.Do(func() { first = true; close(s.entered) })
		if first {
			select {
			case <-s.release:
			case <-time.After(5 * time.Second):
				return row, context.DeadlineExceeded
			}
			if s.uncertain {
				return 0, context.Canceled
			}
		}
	}
	return row, err
}

func (s *receiptBarrierStore) WithdrawPendingUserMessage(ctx context.Context, id, turn string) error {
	return s.Store.(session.PendingInputWithdrawer).WithdrawPendingUserMessage(ctx, id, turn)
}

func TestPublicRootStopWithdrawsFencedLateAppend(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "uncertain"}[uncertain], func(t *testing.T) {
			base := session.NewInMemorySessionStore()
			store := &receiptBarrierStore{Store: base, CoordinationStore: base.(session.CoordinationStore), entered: make(chan struct{}), release: make(chan struct{}), uncertain: uncertain}
			providerEntered, providerStopped := make(chan struct{}), make(chan struct{})
			p := coordinationReply("unused")
			p.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
				close(providerEntered)
				<-ctx.Done()
				close(providerStopped)
				return nil, ctx.Err()
			}
			_, owner := coordinationRuntime(t, store, p, coordinationReply("unused"))
			h := coordinationCreate(t, owner.Runtime(), t.Name(), "")
			_, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
			require.NoError(t, err)
			inputBarrier(t, providerEntered)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			appended := make(chan error, 1)
			go func() { _, err := h.Submit(ctx, TurnInput{Content: "late input", RequestID: "late"}); appended <- err }()
			inputBarrier(t, store.entered)
			cancel()
			require.ErrorIs(t, inputResult(t, appended), context.Canceled)
			stopped := make(chan error, 1)
			go func() { stopped <- h.(SessionTreeController).StopSubtree(t.Context()) }()
			inputBarrier(t, providerStopped)
			close(store.release)
			require.NoError(t, inputResult(t, stopped))
			loaded, err := base.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			turnID, err := sessionInputID(h.ID(), "late")
			require.NoError(t, err)
			require.Equal(t, string(TurnCanceled), loaded.TurnOutcome(turnID))
			restored := newSessionDriver(h.(*sessionHandle).runtime, loaded)
			t.Cleanup(restored.closeOwner)
			require.Empty(t, restored.pending)
			require.NoError(t, owner.Shutdown(t.Context()))
			_, next := coordinationRuntime(t, base, coordinationReply("ok"), coordinationReply("unused"))
			reopened, _, err := next.Runtime().(SessionLoader).LoadSession(t.Context(), h.ID())
			require.NoError(t, err)
			_, err = reopened.Submit(t.Context(), TurnInput{Content: "late input", RequestID: "late"})
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
		})
	}
}

type recallBarrierStore struct {
	session.Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *recallBarrierStore) WithdrawPendingUserMessage(ctx context.Context, id, turn string) error {
	err := s.Store.(session.PendingInputWithdrawer).WithdrawPendingUserMessage(ctx, id, turn)
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if first {
		select {
		case <-s.release:
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return context.DeadlineExceeded
		}
	}
	return err
}

func TestCanceledInputCompletionResultsAreDetached(t *testing.T) {
	for _, kind := range []string{"append", "recall", "guidance"} {
		t.Run(kind, func(t *testing.T) {
			base := session.NewInMemorySessionStore()
			h := pendingRecallHandle(t, base)
			entered, release := make(chan struct{}), make(chan struct{})
			if kind != "append" {
				_, err := h.Steer(t.Context(), TurnInput{Content: "guidance", RequestID: "guidance"})
				require.NoError(t, err)
			}
			switch kind {
			case "append":
				h.driver.r.sessionStore = &receiptBarrierStore{Store: base, entered: entered, release: release}
			case "recall":
				h.driver.r.sessionStore = &recallBarrierStore{Store: base, entered: entered, release: release}
			case "guidance":
				h.driver.r.sessionStore = &promotionBlockedStore{Store: base, entered: entered, release: release}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				switch kind {
				case "append":
					admission, err := h.driver.admitInputReceipt(ctx, QueuedMessage{Content: "late input", RequestID: "late"}, "post", true, false)
					assert.Equal(t, inputAdmission{}, admission)
					result <- err
				case "recall":
					id, _ := sessionInputID(h.ID(), "guidance")
					withdrawn, err := h.CancelPendingMessage(ctx, id)
					assert.False(t, withdrawn)
					result <- err
				case "guidance":
					assert.Empty(t, h.driver.drainInputs(ctx, true))
					result <- ctx.Err()
				}
			}()
			inputBarrier(t, entered)
			cancel()
			require.ErrorIs(t, inputResult(t, result), context.Canceled)
			// Owner remains responsive, but following I/O cannot pass the retained lane.
			_, err := h.Status(t.Context())
			require.NoError(t, err)
			laneCtx, laneCancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			require.ErrorIs(t, h.driver.durableIO(laneCtx, func() (sessionIOReservation, error) { return sessionIOReservation{}, nil }), context.DeadlineExceeded)
			laneCancel()
			close(release)
			require.NoError(t, h.driver.durableIO(t.Context(), func() (sessionIOReservation, error) { return sessionIOReservation{}, nil }))
			loaded, err := base.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			switch kind {
			case "append":
				require.Len(t, loaded.Messages, 1)
			case "recall":
				require.Empty(t, loaded.Messages)
			case "guidance":
				require.False(t, loaded.Messages[0].Message.Pending)
			}
		})
	}
}

type stopRetryStore struct {
	session.Store

	fail bool
}

func (s *stopRetryStore) UpdateSession(ctx context.Context, value *session.Session) error {
	if s.fail {
		return errors.New("withdrawal failed")
	}
	return s.Store.UpdateSession(ctx, value)
}

func TestIdleRootStopRetryClearsOnlyWithdrawalFailure(t *testing.T) {
	for _, settlementFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "withdrawal-only", true: "generation-failure"}[settlementFailure], func(t *testing.T) {
			base := session.NewInMemorySessionStore()
			store := &stopRetryStore{Store: base}
			r := serviceRuntime(t, NewSessionService(), store)
			h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name()), session.WithUserMessage("history")), SessionBinding{})
			require.NoError(t, err)
			generationErr := errors.New("generation failure")
			if settlementFailure {
				require.NoError(t, h.(*sessionHandle).driver.ownerCall(t.Context(), func() error { h.(*sessionHandle).driver.completionErr = generationErr; return nil }))
			}
			store.fail = true
			require.ErrorContains(t, h.(SessionTreeController).StopSubtree(t.Context()), "withdrawal failed")
			store.fail = false
			err = h.(SessionTreeController).StopSubtree(t.Context())
			if settlementFailure {
				require.ErrorIs(t, err, generationErr)
			} else {
				require.NoError(t, err)
			}
			loaded, err := base.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			require.Equal(t, "history", loaded.Messages[0].Message.Message.Content)
		})
	}
}

func TestColdDuplicateCreateLoadsCanonicalRoot(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			r := serviceRuntime(t, NewSessionService(), store)
			// Inspect hydration without starting the restored pending execution.
			r.agents = nil
			r.team = team.New(team.WithAgents(agent.New("root", "prompt", agent.WithMaxIterations(9), agent.WithModel(coordinationReply("unused")))))
			saved := session.New(session.WithID(t.Name()), session.WithAgentName("root"), session.WithUserMessage("saved history"), session.WithMaxIterations(0))
			saved.Origin = "api"
			saved.WorkingDir = "/saved/workspace"
			saved.SetAttribute(SessionAgentAttribute, "root")
			saved.SetAttribute("docker-agent.actor.source", "saved.yaml")
			saved.SetSafetyPolicy(session.SafetyPolicy("confirm"))
			saved.Permissions = &session.PermissionsConfig{Allow: []string{"read_file"}}
			saved.AgentModelOverrides = map[string]string{"root": "test/saved-model"}
			saved.SetTitle("saved title")
			saved.SetTurnOutcome("finished", string(TurnCompleted))
			pending := session.UserMessage("pending")
			pending.Pending, pending.Accepted, pending.TurnID = true, true, "pending"
			saved.AddMessage(pending)
			require.NoError(t, store.AddSession(t.Context(), saved))
			canonical, err := store.GetSession(t.Context(), saved.ID)
			require.NoError(t, err)
			for _, resident := range []bool{false, true} {
				h, err := r.CreateSession(t.Context(), session.New(session.WithID(saved.ID), session.WithOrigin("api")), SessionBinding{AgentName: "root"})
				require.NoError(t, err)
				snapshot, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				require.Equal(t, canonical.MessagesSnapshot(), snapshot.MessagesSnapshot())
				require.Equal(t, canonical.TitleSnapshot(), snapshot.TitleSnapshot())
				require.Equal(t, canonical.SafetyPolicy, snapshot.SafetyPolicy)
				require.Equal(t, canonical.Permissions, snapshot.Permissions)
				require.Equal(t, canonical.MaxIterations, snapshot.MaxIterations)
				require.Equal(t, canonical.WorkingDir, snapshot.WorkingDir)
				require.Equal(t, canonical.Origin, snapshot.Origin)
				require.Equal(t, canonical.AgentModelOverrides, snapshot.AgentModelOverrides)
				require.Equal(t, "test/saved-model", h.Metadata().Model)
				require.Equal(t, string(TurnCompleted), snapshot.TurnOutcome("finished"))
				require.Len(t, h.(*sessionHandle).driver.pending, 1)
				if resident {
					require.NoError(t, r.ReleaseSession(t.Context(), h.ID()))
				}
			}
			for _, conflict := range []string{"origin", "source", "parent", "agent", "model"} {
				template := session.New(session.WithID(saved.ID), session.WithOrigin("api"))
				binding := SessionBinding{AgentName: "root"}
				switch conflict {
				case "origin":
					template.Origin = "other"
				case "source":
					template.SetAttribute("docker-agent.actor.source", "other.yaml")
				case "parent":
					template.ParentID = "other"
				case "agent":
					template.SetAttribute(SessionAgentAttribute, "other")
					binding.AgentName = ""
				case "model":
					binding.Model = "test/other-model"
				}
				_, err := r.CreateSession(t.Context(), template, binding)
				require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
			}
			child := canonical.Clone()
			child.ID = saved.ID + "-child"
			child.ParentID = saved.ID
			require.NoError(t, store.AddSession(t.Context(), child))
			_, err = r.CreateSession(t.Context(), session.New(session.WithID(child.ID)), SessionBinding{AgentName: "root"})
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
		})
	}
}
