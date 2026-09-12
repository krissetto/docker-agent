package runtime

import (
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

// renderTranscript formats a session's conversation as "role: content" lines.
// When limit > 0 only the last `limit` messages are included. System messages
// are skipped; empty messages are omitted.
func renderTranscript(sess *session.Session, limit int) string {
	msgs := sess.GetAllMessages()
	rendered := make([]string, 0, len(msgs))
	for i := range msgs {
		content := strings.TrimSpace(msgs[i].Message.Content)
		if content == "" {
			continue
		}
		rendered = append(rendered, fmt.Sprintf("%s: %s", msgs[i].Message.Role, content))
	}
	if limit > 0 && len(rendered) > limit {
		rendered = rendered[len(rendered)-limit:]
	}
	return strings.Join(rendered, "\n\n")
}
