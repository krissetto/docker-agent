package session

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/chat"
)

// storedMessage keeps request constraints with the existing message JSON. It is
// deliberately not a chat/provider field and needs no conversation-policy edit.
type storedMessage struct {
	chat.Message
	RequireApproval bool `json:"docker_agent_require_approval,omitempty"`
}

func marshalStoredMessage(m *Message) ([]byte, error) {
	return json.Marshal(storedMessage{Message: m.Message, RequireApproval: m.RequireApproval})
}

func unmarshalStoredMessage(data []byte, m *Message) error {
	var stored storedMessage
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	m.Message, m.RequireApproval = stored.Message, stored.RequireApproval
	return nil
}
