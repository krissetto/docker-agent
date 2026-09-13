package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

type schemaFailureStore struct {
	session.Store
	session.CoordinationStore
	session.ItemAppender

	fail atomic.Bool
}

func (s *schemaFailureStore) AppendItem(ctx context.Context, sessionID, writeID string, item session.Item) (int64, error) {
	if s.fail.Load() {
		return 0, errors.New("SQL logic error: no such column: write_hash (1)")
	}
	return s.ItemAppender.AppendItem(ctx, sessionID, writeID, item)
}

func TestPermanentPersistenceFailureIsVisibleAndRejectsNewAdmission(t *testing.T) {
	base := coordinationSQLite(t)
	store := &schemaFailureStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ItemAppender: base.(session.ItemAppender)}
	store.fail.Store(true)
	_, owner := coordinationRuntime(t, store, coordinationReply("first response chunk"), coordinationReply("child"))
	h := coordinationCreate(t, owner.Runtime(), "schema-failure", "")
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	first, err := h.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorContains(t, h.AwaitTurn(ctx, first.TurnID), "write_hash")
	visible := false
	for !visible {
		select {
		case event := <-obs.Events:
			if failure, ok := event.Event.(*ErrorEvent); ok && event.TurnID == first.TurnID {
				assert.Contains(t, failure.Error, "persistence failed")
				visible = true
			}
		case <-ctx.Done():
			t.Fatal("missing correlated persistence ErrorEvent")
		}
	}
	_, err = h.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
	var blocked *SessionError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, SessionErrorPersistence, blocked.Kind)
	assert.Contains(t, blocked.Error(), "write_hash")
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Len(t, snapshot.OwnMessages(), 2, "rejected second input never enters the accepted transcript")
	store.fail.Store(false)
	require.Eventually(t, func() bool { return h.AwaitTurn(t.Context(), first.TurnID) == nil }, 2*time.Second, time.Millisecond)
	second, err := h.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
	require.NoError(t, err)
	coordinationAwait(t, h, second.TurnID)
}
