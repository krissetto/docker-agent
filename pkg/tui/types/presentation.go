package types

import (
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

// HasVisibleProse tests the aggregate text without changing its bytes. Streaming
// whitespace must remain available if a later delta supplies meaningful prose.
func HasVisibleProse(content string) bool { return strings.TrimSpace(content) != "" }

// PresentationHidden suppresses empty prose surfaces, not the underlying turn.
// Media, receipts, outcomes, runtime notices and actual human input keep their
// independent presentation even when there is no prose.
func (m *Message) PresentationHidden() bool {
	if m == nil {
		return false
	}
	switch m.Type {
	case MessageTypeAssistant:
		return !HasVisibleProse(m.Content) && len(m.AssistantMedia) == 0
	case MessageTypeAgentInput:
		return m.InputOrigin == session.InputOriginAgent && !HasVisibleProse(m.Content) &&
			!HasVisibleProse(m.ReceivedBody) && len(m.Attachments) == 0 && !m.Pending && m.ReportOutcome == ""
	}
	return false
}
