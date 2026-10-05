package runtime

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func pendingEdit(turnID, content string, expectedContent ...string) SessionEdit {
	edit := &PendingMessageEdit{TurnID: turnID, Content: content}
	if len(expectedContent) != 0 {
		edit.ExpectedContent = &expectedContent[0]
	}
	return SessionEdit{Kind: SessionEditPendingMessage, PendingMessage: edit}
}

func TestPendingEditSameAdmissionAndOrder(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "steering"}[steer], func(t *testing.T) {
			h := pendingRecallHandle(t, session.NewInMemorySessionStore())
			post := h.Submit
			if steer {
				post = h.Steer
			}
			first, err := post(t.Context(), TurnInput{Content: "first"})
			require.NoError(t, err)
			input := TurnInput{Content: "old", RequestID: "original", MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "old\n\nattachment"}, {Type: chat.MessagePartTypeDocument, Document: &chat.Document{Source: chat.DocumentSource{InlineData: []byte{1, 2}}}}}}
			turn, err := post(t.Context(), input)
			require.NoError(t, err)
			last, err := post(t.Context(), TurnInput{Content: "last"})
			require.NoError(t, err)
			before := h.driver.sess.MessagesSnapshot()
			snapshot, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "new", "old"))
			require.NoError(t, err)
			items := snapshot.MessagesSnapshot()
			require.Len(t, items, 3)
			assert.Equal(t, []string{first.TurnID, turn.TurnID, last.TurnID}, []string{items[0].Message.TurnID, items[1].Message.TurnID, items[2].Message.TurnID})
			assert.Equal(t, session.InputOriginUser, items[1].Message.InputOrigin)
			assert.Equal(t, "new\n\nattachment", items[1].Message.Message.MultiContent[0].Text)
			assert.Equal(t, before[1].Message.Message.CreatedAt, items[1].Message.Message.CreatedAt)
			assert.True(t, items[1].Message.Pending)
			assert.Equal(t, "old", before[1].Message.Message.Content)
			stored, err := h.driver.r.sessionStore.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			assert.Equal(t, "new", stored.MessagesSnapshot()[1].Message.Message.Content)
			_, err = post(t.Context(), input)
			var typed *SessionError
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, SessionErrorConflict, typed.Kind)
			replay := h.driver.events.replay[h.ID()]
			require.Len(t, replay, 4, "three admissions and one edit, no readmission")
			event := replay[3].event.(*PendingUserMessageEditedEvent)
			assert.Equal(t, turn.TurnID, event.TurnID)
			assert.Equal(t, 1, event.SessionPosition)
			assert.Equal(t, session.InputOriginUser, event.InputOrigin)
			items[1].Message.Message.MultiContent[1].Document.Source.InlineData[0] = 9
			assert.Equal(t, byte(1), event.MultiContent[1].Document.Source.InlineData[0])
			if steer {
				drained := h.driver.DrainSteering()
				require.Len(t, drained, 3)
				assert.Equal(t, "new", drained[1].Content)
				_, err = h.Edit(t.Context(), pendingEdit(turn.TurnID, "too late"))
				require.ErrorAs(t, err, &typed)
				assert.Equal(t, SessionErrorStale, typed.Kind)
				assert.Equal(t, "new", drained[1].Content)
			}
		})
	}
}

type pendingEditFailingStore struct {
	session.Store

	err error
}

func (s pendingEditFailingStore) EditPendingUserMessage(context.Context, string, string, string, ...string) error {
	return s.err
}

func TestPendingEditFailureNoMutationOrEvent(t *testing.T) {
	for _, kind := range []string{"persistence", "unsupported", "context", "missing", "active", "nonhuman", "retry", "consumed_prefix", "empty", "layout", "stale_store", "stale_content", "wrong_session"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			h := pendingRecallHandle(t, store)
			turn, err := h.Steer(t.Context(), TurnInput{Content: "old"})
			require.NoError(t, err)
			ctx := t.Context()
			edit := pendingEdit(turn.TurnID, "new")
			want := SessionErrorStale
			switch kind {
			case "persistence":
				h.driver.r.sessionStore = pendingEditFailingStore{Store: store, err: assert.AnError}
				want = SessionErrorPersistence
			case "stale_store":
				h.driver.r.sessionStore = pendingEditFailingStore{Store: store, err: session.ErrPendingMessageStale}
			case "unsupported":
				h.driver.r.sessionStore = struct{ session.Store }{store}
				want = SessionErrorUnsupported
			case "context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "stale_content":
				expected := "older"
				edit.PendingMessage.ExpectedContent = &expected
			case "wrong_session":
				h.driver.sess.ID = "other"
			case "missing":
				edit.PendingMessage.TurnID = "missing"
			case "active":
				h.driver.activeRequestID = turn.TurnID
			case "nonhuman":
				h.driver.steering[0].InputOrigin = session.InputOriginAgent
			case "retry":
				h.driver.steering[0].Retry = true
			case "consumed_prefix":
				msg := session.UserMessage("consumed")
				msg.TurnID, msg.Accepted, msg.InputOrigin = turn.TurnID, true, session.InputOriginUser
				h.driver.sess.AddMessage(msg)
			case "empty":
				edit.PendingMessage.Content = "  "
				want = SessionErrorInvalid
			case "layout":
				h.driver.sess.Messages[0].Message.Message.MultiContent = []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: "unrelated"}}
				want = SessionErrorUnsupported
			}
			before := h.driver.sess.MessagesSnapshot()
			_, err = h.Edit(ctx, edit)
			if kind == "context" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				var typed *SessionError
				require.ErrorAs(t, err, &typed)
				assert.Equal(t, want, typed.Kind)
				assert.Equal(t, SessionOperation("edit_pending_message"), typed.Operation)
			}
			assert.Equal(t, before, h.driver.sess.MessagesSnapshot())
			assert.Equal(t, "old", h.driver.steering[0].Content)
			assert.Len(t, h.driver.events.replay[h.ID()], 1)
		})
	}
}

func TestPendingEditSQLiteRestartTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-edit.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	h := pendingRecallHandle(t, store)
	turn, err := h.Submit(t.Context(), TurnInput{Content: "old"})
	require.NoError(t, err)
	_, err = h.Edit(t.Context(), pendingEdit(turn.TurnID, "new", "old"))
	require.NoError(t, err)
	require.NoError(t, store.Close())
	for range 2 {
		reopened, err := sqlitestore.New(t.Context(), path)
		require.NoError(t, err)
		loaded, err := reopened.GetSession(t.Context(), h.ID())
		require.NoError(t, err)
		r := newDriverTestRuntime(t)
		r.sessionStore = reopened
		d := newSessionDriver(r, loaded)
		require.Len(t, d.pending, 1)
		assert.Equal(t, turn.TurnID, d.pending[0].RequestID)
		assert.Equal(t, "new", d.pending[0].Content)
		assert.Empty(t, d.events.replay[h.ID()], "restart restores payload, not edit history")
		require.NoError(t, reopened.Close())
	}
}

type pendingEditBarrierStore struct {
	session.Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pendingEditBarrierStore) EditPendingUserMessage(ctx context.Context, sessionID, turnID, content string, expectedContent ...string) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.Store.(session.PendingMessageEditor).EditPendingUserMessage(ctx, sessionID, turnID, content, expectedContent...)
}

func (s *pendingEditBarrierStore) DeletePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	return s.Store.(session.PendingMessageDeleter).DeletePendingUserMessage(ctx, sessionID, turnID)
}

func TestPendingEditLinearizesDrainAndCancel(t *testing.T) {
	for _, transition := range []string{"drain", "cancel", "start"} {
		t.Run(transition, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			h := pendingRecallHandle(t, store)
			turn, err := h.Steer(t.Context(), TurnInput{Content: "old"})
			require.NoError(t, err)
			if transition == "start" {
				h.driver.pending, h.driver.steering = h.driver.steering, nil
				h.driver.leave(sessionRunning)
				h.driver.activeRequestID = ""
			}
			barrier := &pendingEditBarrierStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
			h.driver.r.sessionStore = barrier
			edited := make(chan error, 1)
			go func() { _, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "new", "old")); edited <- err }()
			<-barrier.entered
			done := make(chan error, 1)
			var drained []QueuedMessage
			go func() {
				switch transition {
				case "drain":
					drained = h.driver.DrainSteering()
					done <- nil
				case "cancel":
					_, err := h.CancelPendingMessage(t.Context(), turn.TurnID)
					done <- err
				case "start":
					_, _, _, err := h.driver.prepareStart(t.Context(), true)
					done <- err
				}
			}()
			close(barrier.release)
			require.NoError(t, <-edited)
			require.NoError(t, <-done)
			if transition == "drain" {
				require.Len(t, drained, 1)
				assert.Equal(t, "new", drained[0].Content)
			}
			if transition == "start" {
				h.driver.cancel()
				h.driver.wg.Done()
			}
			_, err = h.Edit(t.Context(), pendingEdit(turn.TurnID, "late", "new"))
			var typed *SessionError
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, SessionErrorStale, typed.Kind)
			assert.Empty(t, h.driver.pending)
			assert.Empty(t, h.driver.steering)
		})
	}
}

func TestPendingEditConcurrentLastWriterWins(t *testing.T) {
	store := session.NewInMemorySessionStore()
	h := pendingRecallHandle(t, store)
	turn, err := h.Steer(t.Context(), TurnInput{Content: "old"})
	require.NoError(t, err)
	barrier := &pendingEditBarrierStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	h.driver.r.sessionStore = barrier
	first := make(chan error, 1)
	go func() { _, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "first")); first <- err }()
	<-barrier.entered
	second := make(chan error, 1)
	go func() { _, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "second")); second <- err }()
	close(barrier.release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.Equal(t, "second", h.driver.steering[0].Content)
	stored, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	assert.Equal(t, "second", stored.MessagesSnapshot()[0].Message.Message.Content)
	require.Len(t, h.driver.events.replay[h.ID()], 3)
	assert.Equal(t, "first", h.driver.events.replay[h.ID()][1].event.(*PendingUserMessageEditedEvent).Message)
	assert.Equal(t, "second", h.driver.events.replay[h.ID()][2].event.(*PendingUserMessageEditedEvent).Message)
}

func TestPendingEditConcurrentStaleDraftRejected(t *testing.T) {
	store := session.NewInMemorySessionStore()
	h := pendingRecallHandle(t, store)
	turn, err := h.Steer(t.Context(), TurnInput{Content: "old"})
	require.NoError(t, err)
	barrier := &pendingEditBarrierStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	h.driver.r.sessionStore = barrier
	first := make(chan error, 1)
	go func() { _, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "first", "old")); first <- err }()
	<-barrier.entered
	second := make(chan error, 1)
	go func() { _, err := h.Edit(t.Context(), pendingEdit(turn.TurnID, "second", "old")); second <- err }()
	close(barrier.release)
	require.NoError(t, <-first)
	var typed *SessionError
	require.ErrorAs(t, <-second, &typed)
	assert.Equal(t, SessionErrorStale, typed.Kind)
	assert.Equal(t, "first", h.driver.steering[0].Content)
	assert.Equal(t, "first", h.driver.sess.MessagesSnapshot()[0].Message.Message.Content)
	stored, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	assert.Equal(t, "first", stored.MessagesSnapshot()[0].Message.Message.Content)
	assert.Len(t, h.driver.events.replay[h.ID()], 2, "stale edits publish nothing")
}

func TestPendingEditRejectsDivergedStore(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if kind == "sqlite" {
				var err error
				store, err = sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "edit.db"))
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			h := pendingRecallHandle(t, store)
			turn, err := h.Submit(t.Context(), TurnInput{Content: "old"})
			require.NoError(t, err)
			require.NoError(t, store.(session.PendingMessageEditor).EditPendingUserMessage(t.Context(), h.ID(), turn.TurnID, "newer", "old"))
			before := h.driver.sess.MessagesSnapshot()
			_, err = h.Edit(t.Context(), pendingEdit(turn.TurnID, "stale draft", "old"))
			var typed *SessionError
			require.ErrorAs(t, err, &typed)
			assert.Equal(t, SessionErrorStale, typed.Kind)
			assert.Equal(t, before, h.driver.sess.MessagesSnapshot())
			assert.Equal(t, "old", h.driver.pending[0].Content)
			assert.Len(t, h.driver.events.replay[h.ID()], 1)
			stored, err := store.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			assert.Equal(t, "newer", stored.MessagesSnapshot()[0].Message.Message.Content)
		})
	}
}

func TestPendingEditSameContentAndTextCAS(t *testing.T) {
	h := pendingRecallHandle(t, session.NewInMemorySessionStore())
	turn, err := h.Steer(t.Context(), TurnInput{Content: "A"})
	require.NoError(t, err)
	for _, change := range [][2]string{{"A", "A"}, {"A", "B"}, {"B", "A"}, {"A", "C"}} {
		_, err = h.Edit(t.Context(), pendingEdit(turn.TurnID, change[1], change[0]))
		require.NoError(t, err)
	}
	assert.Equal(t, "C", h.driver.steering[0].Content)
	assert.Len(t, h.driver.events.replay[h.ID()], 5, "same-content saves retain successful edit event semantics")
}
