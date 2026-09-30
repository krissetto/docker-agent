package runtime

import (
	"context"
	"errors"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

// The owner lock linearizes edits with steering drain, start, handoff and recall.
func (h *sessionHandle) editPendingMessageLocked(ctx context.Context, edit *PendingMessageEdit) (*session.Session, error) {
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
		return nil, failure(SessionErrorInvalid, "turn_id is required")
	}
	if d.sess == nil || d.sess.ID != h.sessionID || edit.TurnID == d.activeRequestID {
		return nil, stale()
	}
	queue := &d.pending
	index := slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == edit.TurnID })
	if index < 0 {
		queue = &d.steering
		index = slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == edit.TurnID })
	}
	if index < 0 || (*queue)[index].Retry || (*queue)[index].InputOrigin != session.InputOriginUser {
		return nil, stale()
	}
	position := -1
	var current *session.Message
	for i, item := range d.sess.MessagesSnapshot() {
		if item.Message == nil || item.Message.TurnID != edit.TurnID {
			continue
		}
		// Any consumed prefix or duplicate identity makes the whole turn immutable.
		if current != nil || !item.Message.Pending {
			return nil, stale()
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
		return nil, classify(err)
	}
	if (*queue)[index].AcceptedPersisted {
		store, ok := d.r.sessionStore.(session.PendingMessageEditor)
		if !ok {
			return nil, failure(SessionErrorUnsupported, "store does not support pending message edits")
		}
		if err := store.EditPendingUserMessage(ctx, h.sessionID, edit.TurnID, edit.Content, expectedContent...); err != nil {
			return nil, classify(err)
		}
	} else if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Persistence has committed. These replacements cannot fail under d.mu;
	// the memory store may already have replaced the same live slot.
	d.sess.ReplacePendingUserMessagePayload(edit.TurnID, next.Message.Content, next.Message.MultiContent)
	msg := &(*queue)[index]
	msg.Content, msg.MultiContent = next.Message.Content, next.Message.MultiContent
	eventPayload := d.sess.MessagesSnapshot()[position].Message.Message
	d.events.PublishForRequest(h.sessionID, edit.TurnID, inputEventMetadata(PendingUserMessageEdited(h.sessionID, edit.TurnID, eventPayload.Content, eventPayload.MultiContent, position), *msg))
	return d.sess.Clone(), nil
}
