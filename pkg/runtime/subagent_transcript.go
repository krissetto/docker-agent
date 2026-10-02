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
	start := 0
	if limit > 0 {
		remaining := limit
		start = len(msgs)
		for start > 0 && remaining > 0 {
			start--
			if strings.TrimSpace(msgs[start].Message.Content) != "" {
				remaining--
			}
		}
	}
	capacity := len(msgs) - start
	if limit > 0 {
		capacity = min(capacity, limit)
	}
	rendered := make([]string, 0, capacity)
	for i := start; i < len(msgs); i++ {
		content := strings.TrimSpace(msgs[i].Message.Content)
		if content == "" {
			continue
		}
		rendered = append(rendered, fmt.Sprintf("%s: %s", msgs[i].Message.Role, content))
	}
	return strings.Join(rendered, "\n\n")
}
