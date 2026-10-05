package runtime

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type independentChildAdmissionBarrier struct {
	session.Store
	session.CoordinationStore
	session.ItemAppender
	session.ChildCommitBatchStore

	entered chan struct{}
	release chan struct{}
	lostAck bool
	once    sync.Once
}

func (s *independentChildAdmissionBarrier) AdmitChild(ctx context.Context, a session.ChildAdmission) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := s.CoordinationStore.AdmitChild(ctx, a)
	if err == nil && s.lostAck {
		return errors.New("admission ack lost")
	}
	return err
}

func TestRootStopAccountsForUnpublishedAdmission(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, lostAck := range []bool{false, true} {
			t.Run(kind+"/"+strconv.FormatBool(lostAck), func(t *testing.T) {
				base := session.NewInMemorySessionStore()
				if kind == "sqlite" {
					base = coordinationSQLite(t)
				}
				store := &independentChildAdmissionBarrier{Store: base, CoordinationStore: base.(session.CoordinationStore), ItemAppender: base.(session.ItemAppender), ChildCommitBatchStore: base.(session.ChildCommitBatchStore), entered: make(chan struct{}), release: make(chan struct{}), lostAck: lostAck}
				_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
				root := coordinationCreate(t, owner.Runtime(), "fenced-root", "")
				created := make(chan error, 1)
				go func() {
					_, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("fenced-child")), SessionBinding{AgentName: "worker", ParentSessionID: root.ID()})
					created <- err
				}()
				coordinationWait(t, store.entered)
				stopCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, root.(SessionTreeController).StopSubtree(stopCtx), context.DeadlineExceeded, "stop must not acknowledge an unresolved admission")
				close(store.release)
				require.ErrorIs(t, <-created, &SessionError{Kind: SessionErrorStopped})
				require.NoError(t, root.(SessionTreeController).StopSubtree(t.Context()), "timed out fence remains retryable")
				records, err := store.LoadChildren(t.Context(), root.ID())
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, subagent.NodeStopped, records[0].Node.State, "late durable admission must not recover executable")
				child, _, err := owner.Runtime().(SessionLoader).LoadSession(t.Context(), "fenced-child")
				if err == nil {
					_, err = child.Submit(t.Context(), TurnInput{Content: "must not execute", RequestID: "after-stop"})
				}
				require.Error(t, err)
			})
		}
	}
}

func TestCanceledInputRetryCannotReexecute(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				store = coordinationSQLite(t)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var mu sync.Mutex
			var calls int
			var secondExecuted bool
			provider := coordinationReply("unused")
			provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
				mu.Lock()
				calls++
				first := calls == 1
				for _, msg := range messages {
					if msg.Content == "withdrawn B" {
						secondExecuted = true
					}
				}
				mu.Unlock()
				if first {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return newStreamBuilder().AddContent("reply").AddStopWithUsage(1, 1).Build(), nil
			}
			_, owner := coordinationRuntime(t, store, provider, coordinationReply("worker"))
			h := coordinationCreate(t, owner.Runtime(), "independent-cancel-"+kind, "")
			a, err := h.Submit(t.Context(), TurnInput{Content: "blocked A", RequestID: "A"})
			require.NoError(t, err)
			coordinationWait(t, entered)
			b, err := h.Submit(t.Context(), TurnInput{Content: "withdrawn B", RequestID: "B"})
			require.NoError(t, err)
			cancelResult, err := h.Cancel(t.Context(), b.TurnID)
			require.NoError(t, err)
			require.Equal(t, CancelAccepted, cancelResult.Outcome)
			t.Logf("accepted B identity=%s; public Cancel returned %s", b.TurnID, cancelResult.Outcome)
			retry, retryErr := h.Submit(t.Context(), TurnInput{Content: "withdrawn B", RequestID: "B"})
			t.Logf("identical retry submission=%+v err=%v", retry, retryErr)
			require.ErrorIs(t, retryErr, &SessionError{Kind: SessionErrorConflict})
			unblock()
			coordinationAwait(t, h, a.TurnID)
			var status SessionStatus
			require.Eventually(t, func() bool {
				status, err = h.Status(t.Context())
				require.NoError(t, err)
				return status.State == SessionStateSettled && status.Pending == 0
			}, time.Second, time.Millisecond)
			t.Logf("post-A status=%+v", status)
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			stored, err := store.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			mu.Lock()
			observedCalls, executed := calls, secondExecuted
			mu.Unlock()
			liveB, durableB := 0, 0
			for _, item := range snapshot.MessagesSnapshot() {
				if item.Message != nil && item.Message.TurnID == b.TurnID {
					liveB++
				}
			}
			for _, item := range stored.MessagesSnapshot() {
				if item.Message != nil && item.Message.TurnID == b.TurnID {
					durableB++
				}
			}
			t.Logf("RESULT calls=%d canceled_B_in_provider_input=%t live_B_rows=%d durable_B_rows=%d", observedCalls, executed, liveB, durableB)
			require.False(t, executed, "a transport retry must not reexecute an explicitly withdrawn immutable request identity")
			require.Equal(t, 1, observedCalls, "only original A may execute")
			require.Equal(t, 0, status.Pending, "withdrawn identity retry must not create phantom pending input")
		})
	}
}

type lostWithdrawalAckStore struct {
	session.Store
	session.ItemAppender
	session.PendingInputWithdrawer

	once sync.Once
}

func (s *lostWithdrawalAckStore) WithdrawPendingUserMessage(ctx context.Context, id, turn string) error {
	err := s.PendingInputWithdrawer.WithdrawPendingUserMessage(ctx, id, turn)
	if err != nil {
		return err
	}
	s.once.Do(func() { err = errors.New("withdrawal ack lost") })
	return err
}

func TestCanceledInputLostWithdrawalAcknowledgment(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			base := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				base = coordinationSQLite(t)
			}
			store := &lostWithdrawalAckStore{Store: base, ItemAppender: base.(session.ItemAppender), PendingInputWithdrawer: base.(session.PendingInputWithdrawer)}
			h := pendingRecallHandle(t, store)
			input := TurnInput{Content: "withdraw", RequestID: "stable"}
			turn, err := h.Submit(t.Context(), input)
			require.NoError(t, err)
			result, err := h.Cancel(t.Context(), turn.TurnID)
			require.NoError(t, err)
			require.Equal(t, CancelAccepted, result.Outcome)
			_, err = h.Submit(t.Context(), input)
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
			input.Content = "different"
			_, err = h.Submit(t.Context(), input)
			require.ErrorIs(t, err, &SessionError{Kind: SessionErrorConflict})
			require.False(t, h.driver.HasPending())
		})
	}
}
