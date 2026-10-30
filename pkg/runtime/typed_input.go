package runtime

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func normalizedInputOrigin(origin session.InputOrigin) session.InputOrigin {
	if origin == session.InputOriginAgent || origin == session.InputOriginRuntime {
		return origin
	}
	return session.InputOriginUser
}

type deliveryClass uint8

const (
	deliveryTurn deliveryClass = iota
	deliveryGuidance
)

// Delivery follows the persisted mode. Origin is provenance only.
func (msg QueuedMessage) deliveryClass() deliveryClass {
	if msg.InputMode == "steer" {
		return deliveryGuidance
	}
	return deliveryTurn
}

func (msg QueuedMessage) guidance() bool { return msg.deliveryClass() == deliveryGuidance }

func inputMode(mode string) string {
	if mode == "" {
		return "turn"
	}
	return mode
}

func (msg QueuedMessage) sessionMessage() *session.Message {
	message := session.UserMessage(msg.Content, msg.MultiContent...)
	message.Message.MessageID = msg.MessageID
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
		event.MessageID = msg.MessageID
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

// userMessageIDFor derives the identity of a user message row from its request
// ID and its attempt kind ("submit", "retry" or "steer"). A redelivered
// submission keeps its identity, so the store sees identical content. A retry
// of the same request is a different attempt and therefore a different row.
// The ID never comes from content or position.
func userMessageIDFor(requestID, attempt string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("docker-agent/user-message/"+attempt+"/"+requestID)).String()
}
