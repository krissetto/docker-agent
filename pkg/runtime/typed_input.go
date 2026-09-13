package runtime

import (
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func normalizedInputOrigin(origin session.InputOrigin) session.InputOrigin {
	if origin == session.InputOriginAgent || origin == session.InputOriginRuntime {
		return origin
	}
	return session.InputOriginUser
}

func (msg QueuedMessage) trustedSteering() bool {
	return msg.InputMode == "steer" && (msg.InputOrigin == session.InputOriginAgent || msg.InputOrigin == session.InputOriginRuntime)
}

func inputMode(mode string) string {
	if mode == "" {
		return "turn"
	}
	return mode
}

func (msg QueuedMessage) sessionMessage() *session.Message {
	message := session.UserMessage(msg.Content, msg.MultiContent...)
	message.InputOrigin, message.SenderID, message.SenderName = msg.InputOrigin, msg.SenderID, msg.SenderName
	message.InputMode = msg.InputMode
	if message.InputMode == "" {
		message.InputMode = "turn"
	}
	return message
}

func queuedSessionInput(message *session.Message, position int, persisted bool) QueuedMessage {
	return QueuedMessage{
		Content: message.Message.Content, MultiContent: message.Message.MultiContent,
		RequestID: message.TurnID, AcceptedPosition: position, AcceptedPersisted: persisted,
		InputOrigin: message.InputOrigin, SenderID: message.SenderID, SenderName: message.SenderName, InputMode: message.InputMode,
	}
}

func inputEventMetadata(event Event, msg QueuedMessage) Event {
	switch event := event.(type) {
	case *PendingUserMessageAcceptedEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
	case *PendingUserMessagePromotedEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
	case *PendingUserMessageCanceledEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
	case *UserMessageEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.TurnID = msg.RequestID
	}
	return event
}

// Attribution is applied only to the assembler's detached message snapshot.
func projectModelInput(message *session.Message) {
	if message.Message.Role != chat.MessageRoleUser {
		return
	}
	switch message.InputOrigin {
	case session.InputOriginAgent:
		message.Message.Content = systemInfo(fmt.Sprintf("Message from agent %q (%s):\n\n%s", message.SenderName, message.SenderID, message.Message.Content))
	case session.InputOriginRuntime:
		message.Message.Content = systemInfo(message.Message.Content)
	}
}

// Trusted steering shares the durable pending mailbox, but is drained at the
// active turn's next safe boundary rather than behind older user turns.
func (d *sessionDriver) postTrustedInput(ctx context.Context, msg QueuedMessage) bool {
	if msg.RequestID == "" {
		id, err := newSessionRequestID()
		if err != nil {
			return false
		}
		msg.RequestID = id
	}
	d.mu.Lock()
	if found, _, err := d.existingInputLocked(msg); found || err != nil {
		d.mu.Unlock()
		return err == nil
	}
	if ctx.Err() != nil || d.admitLocked(SessionOperationPost) != nil || !limitAllows(len(d.pending), d.pendingLimit()) {
		d.mu.Unlock()
		return false
	}
	if err := d.acceptInputLocked(&msg); err != nil {
		d.mu.Unlock()
		return false
	}
	d.pending = append(d.pending, msg)
	d.refreshSteeringLocked()
	d.mu.Unlock()
	d.WakePending()
	return true
}
