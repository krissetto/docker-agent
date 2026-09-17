package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func pendingRecallHandle(t *testing.T, store session.Store) *sessionHandle {
	t.Helper()
	r := newDriverTestRuntime(t)
	r.sessionStore = store
	sess := session.New(session.WithID("recall"))
	if store != nil {
		// Keep durable and live transcripts distinct, as in production loads.
		require.NoError(t, store.AddSession(t.Context(), sess.Clone()))
	}
	d := r.sessionDrivers.Get(sess)
	d.events = newSessionEventHubWithLimits(32, 1<<20)
	d.phase = sessionRunning // retain submissions without invoking a provider
	d.activeRequestID = "active"
	return &sessionHandle{runtime: r, driver: d, sessionID: sess.ID}
}

func TestPendingRecallPendingAndSteering(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "steering"}[steer], func(t *testing.T) {
			h := pendingRecallHandle(t, session.NewInMemorySessionStore())
			d := h.driver
			var turn Submission
			var err error
			if steer {
				turn, err = h.Steer(t.Context(), TurnInput{Content: "withdraw"})
			} else {
				turn, err = h.Submit(t.Context(), TurnInput{Content: "withdraw"})
			}
			require.NoError(t, err)
			for _, id := range []string{"", "unknown", "active"} {
				ok, err := h.CancelPendingMessage(t.Context(), id)
				require.NoError(t, err)
				assert.False(t, ok)
			}
			ok, err := h.CancelPendingMessage(t.Context(), turn.TurnID)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Empty(t, d.pending)
			assert.Empty(t, d.steering)
			assert.False(t, d.interruptRequested)
			assert.Empty(t, d.sess.MessagesSnapshot())
			stored, err := d.r.sessionStore.GetSession(t.Context(), h.sessionID)
			require.NoError(t, err)
			assert.Empty(t, stored.MessagesSnapshot())
			_, _, _, pending, _ := d.snapshotLocked()
			assert.Empty(t, pending)
			cursor := uint64(0)
			replay, _, cancel, _ := d.events.SubscribeSequenced(h.sessionID, &cursor, 8)
			defer cancel()
			require.Len(t, replay, 2)
			canceled, ok := replay[1].Event.(*PendingUserMessageCanceledEvent)
			require.True(t, ok)
			assert.Equal(t, turn.TurnID, canceled.TurnID)
			assert.Equal(t, turn.TurnID, replay[1].RequestID)
			assert.Zero(t, canceled.SessionPosition)
			ok, err = h.CancelPendingMessage(t.Context(), turn.TurnID)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

type pendingRecallFailingStore struct{ session.Store }

func (pendingRecallFailingStore) DeletePendingUserMessage(context.Context, string, string) error {
	return assert.AnError
}

func TestPendingRecallFailureLeavesAcceptedInput(t *testing.T) {
	for _, kind := range []string{"persistence", "unsupported", "context", "stored_child"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			h := pendingRecallHandle(t, store)
			turn, err := h.Steer(t.Context(), TurnInput{Content: "keep"})
			require.NoError(t, err)
			ctx := t.Context()
			switch kind {
			case "persistence":
				h.driver.r.sessionStore = pendingRecallFailingStore{Store: store}
			case "unsupported":
				h.driver.r.sessionStore = struct{ session.Store }{store}
			case "context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "stored_child":
				h.driver.sess.ParentID = "parent"
			}
			ok, err := h.CancelPendingMessage(ctx, turn.TurnID)
			if kind == "stored_child" {
				require.NoError(t, err)
				require.True(t, ok)
				require.Empty(t, h.driver.steering)
				return
			}
			require.Error(t, err)
			assert.False(t, ok)
			if kind == "unsupported" || kind == "stored_child" {
				var sessionErr *SessionError
				require.ErrorAs(t, err, &sessionErr)
				assert.Equal(t, SessionErrorUnsupported, sessionErr.Kind)
			}
			require.Len(t, h.driver.steering, 1)
			assert.True(t, h.driver.interruptRequested)
			items := h.driver.sess.MessagesSnapshot()
			require.Len(t, items, 1)
			assert.True(t, items[0].Message.Pending)
			assert.Len(t, h.driver.events.replay[h.sessionID], 1, "no success event on failure")
			stored, err := store.GetSession(t.Context(), h.sessionID)
			require.NoError(t, err)
			assert.Len(t, stored.MessagesSnapshot(), 1)
		})
	}
}

func TestPendingRecallSQLiteRestoreDoesNotReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recall.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	h := pendingRecallHandle(t, store)
	turn, err := h.Submit(t.Context(), TurnInput{Content: "never replay"})
	require.NoError(t, err)
	ok, err := h.CancelPendingMessage(t.Context(), turn.TurnID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.Close())
	for range 2 {
		reopened, err := sqlitestore.New(t.Context(), path)
		require.NoError(t, err)
		loaded, err := reopened.GetSession(t.Context(), h.sessionID)
		require.NoError(t, err)
		assert.Empty(t, loaded.MessagesSnapshot())
		r := newDriverTestRuntime(t)
		r.sessionStore = reopened
		restored := newSessionDriver(r, loaded)
		assert.False(t, restored.HasPending(), "restore must not re-admit recalled input")
		assert.Empty(t, restored.DrainSteering())
		require.NoError(t, reopened.Close())
	}
}

func TestPendingRecallSerializesWithPromotion(t *testing.T) {
	for _, steering := range []bool{false, true} {
		for range 50 {
			h := pendingRecallHandle(t, nil)
			d := h.driver
			turn, err := h.Steer(t.Context(), TurnInput{Content: "race"})
			require.NoError(t, err)
			if !steering {
				d.pending, d.steering = d.steering, nil
			}
			var recalled, promoted bool
			var recallErr, promotionErr error
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Go(func() {
				<-start
				recalled, recallErr = h.CancelPendingMessage(t.Context(), turn.TurnID)
			})
			wg.Go(func() {
				<-start
				if steering {
					promoted = len(d.DrainSteering()) == 1
					return
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if len(d.pending) != 0 {
					promotionErr = d.promoteInputLocked(d.pending[0])
					if promotionErr == nil {
						d.pending = d.pending[1:]
						d.activeRequestID = turn.TurnID
						promoted = true
					}
				}
			})
			close(start)
			wg.Wait()
			require.NoError(t, recallErr)
			require.NoError(t, promotionErr)
			assert.NotEqual(t, recalled, promoted, "exactly one transition wins")
			items := d.sess.MessagesSnapshot()
			if recalled {
				assert.Empty(t, items)
			} else {
				require.Len(t, items, 1)
				assert.False(t, items[0].Message.Pending)
				ok, err := h.CancelPendingMessage(t.Context(), turn.TurnID)
				require.NoError(t, err)
				assert.False(t, ok)
			}
		}
	}
}

func TestPendingRecallVolatileChild(t *testing.T) {
	h := pendingRecallHandle(t, nil)
	h.driver.sess.ParentID = "parent"
	turn, err := h.Steer(t.Context(), TurnInput{Content: "volatile"})
	require.NoError(t, err)
	ok, err := h.CancelPendingMessage(t.Context(), turn.TurnID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Empty(t, h.driver.sess.MessagesSnapshot())
}
