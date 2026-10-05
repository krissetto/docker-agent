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
	message.ReportOutcome = msg.ReportOutcome
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
		InputOrigin: message.InputOrigin, SenderID: message.SenderID, SenderName: message.SenderName, ReportOutcome: message.ReportOutcome, InputMode: message.InputMode,
	}
}

func inputEventMetadata(event Event, msg QueuedMessage) Event {
	switch event := event.(type) {
	case *PendingUserMessageAcceptedEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.ReportOutcome = msg.ReportOutcome
	case *PendingUserMessageEditedEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.ReportOutcome = msg.ReportOutcome
	case *PendingUserMessagePromotedEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.ReportOutcome = msg.ReportOutcome
	case *PendingUserMessageCanceledEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.ReportOutcome = msg.ReportOutcome
	case *UserMessageEvent:
		event.InputOrigin, event.SenderID, event.SenderName, event.InputMode = msg.InputOrigin, msg.SenderID, msg.SenderName, msg.InputMode
		event.ReportOutcome = msg.ReportOutcome
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
	_, err := d.admitInput(ctx, msg, SessionOperationPost, true, false)
	if err != nil {
		return false
	}
	d.WakePending()
	return true
}
