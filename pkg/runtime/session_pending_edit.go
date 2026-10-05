package runtime

import (
	"context"
	"errors"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

func (h *sessionHandle) editPendingMessage(ctx context.Context, edit *PendingMessageEdit) (*session.Session, error) {
	var snapshot *session.Session
	err := h.driver.durableIO(ctx, func() (sessionIOReservation, error) {
		return h.reservePendingMessageEdit(edit, &snapshot)
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (h *sessionHandle) reservePendingMessageEdit(edit *PendingMessageEdit, snapshot **session.Session) (sessionIOReservation, error) {
	d := h.driver
	failure := func(kind SessionErrorKind, detail string) error {
		turnID := ""
		if edit != nil {
			turnID = edit.TurnID
		}
		return &SessionError{Kind: kind, SessionID: h.sessionID, RequestID: turnID, Operation: "edit_pending_message", Detail: detail}
	}
	stale := func() error {
		return failure(SessionErrorStale, session.ErrPendingMessageStale.Error())
	}
	if edit == nil || edit.TurnID == "" {
		return sessionIOReservation{}, failure(SessionErrorInvalid, "turn_id is required")
	}
	if d.sess == nil || d.sess.ID != h.sessionID || edit.TurnID == d.activeRequestID {
		return sessionIOReservation{}, stale()
	}
	queue := &d.pending
	index := slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == edit.TurnID })
	if index < 0 {
		queue = &d.steering
		index = slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == edit.TurnID })
	}
	if index < 0 || (*queue)[index].Retry || (*queue)[index].InputOrigin != session.InputOriginUser {
		return sessionIOReservation{}, stale()
	}
	position := -1
	var current *session.Message
	for i, item := range d.sess.MessagesSnapshot() {
		if item.Message == nil || item.Message.TurnID != edit.TurnID {
			continue
		}
		// Any consumed prefix or duplicate identity makes the whole turn immutable.
		if current != nil || !item.Message.Pending {
			return sessionIOReservation{}, stale()
		}
		current, position = item.Message, i
	}
	var expectedContent []string
	if edit.ExpectedContent != nil {
		expectedContent = []string{*edit.ExpectedContent}
	}
	next, err := session.PendingUserMessageReplacement(current, edit.Content, expectedContent...)
	classify := func(err error) error {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case errors.Is(err, session.ErrPendingMessageStale), errors.Is(err, session.ErrNotFound):
			return stale()
		case errors.Is(err, session.ErrPendingMessageLayout):
			return failure(SessionErrorUnsupported, err.Error())
		case errors.Is(err, session.ErrPendingMessageEmpty):
			return failure(SessionErrorInvalid, err.Error())
		default:
			return failure(SessionErrorPersistence, err.Error())
		}
	}
	if err != nil {
		return sessionIOReservation{}, classify(err)
	}
	var write func(context.Context) error
	if (*queue)[index].AcceptedPersisted {
		store, ok := d.r.sessionStore.(session.PendingMessageEditor)
		if !ok {
			return sessionIOReservation{}, failure(SessionErrorUnsupported, "store does not support pending message edits")
		}
		// CAS always uses the reserved payload, including callers that omit a CAS.
		expected := current.Message.Content
		write = func(ctx context.Context) error {
			return store.EditPendingUserMessage(ctx, h.sessionID, edit.TurnID, edit.Content, expected)
		}
	}
	d.editReserved = true
	return sessionIOReservation{write: write, commit: func(err error) error {
		d.editReserved = false
		if err != nil {
			return classify(err)
		}
		// Promotion and recall cannot consume the reserved slot before publication.
		if index >= len(*queue) || (*queue)[index].RequestID != edit.TurnID {
			return stale()
		}
		d.sess.ReplacePendingUserMessagePayload(edit.TurnID, next.Message.Content, next.Message.MultiContent)
		msg := &(*queue)[index]
		msg.Content, msg.MultiContent = next.Message.Content, next.Message.MultiContent
		eventPayload := d.sess.MessagesSnapshot()[position].Message.Message
		d.events.PublishForRequest(h.sessionID, edit.TurnID, inputEventMetadata(PendingUserMessageEdited(h.sessionID, edit.TurnID, eventPayload.Content, eventPayload.MultiContent, position), *msg))
		*snapshot = d.sess.Clone()
		d.r.sessionDrivers.signalWork()
		return nil
	}}, nil
}
