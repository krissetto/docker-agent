package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// Retain the original formatter as an equivalence and performance baseline.
func renderTranscriptFullHistory(sess *session.Session, limit int) string {
	msgs := sess.GetAllMessages()
	rendered := make([]string, 0, len(msgs))
	for i := range msgs {
		content := strings.TrimSpace(msgs[i].Message.Content)
		if content != "" {
			rendered = append(rendered, fmt.Sprintf("%s: %s", msgs[i].Message.Role, content))
		}
	}
	if limit > 0 && len(rendered) > limit {
		rendered = rendered[len(rendered)-limit:]
	}
	return strings.Join(rendered, "\n\n")
}

func TestRenderTranscriptTailMatchesFullHistory(t *testing.T) {
	sess := session.New()
	sess.AddMessage(session.UserMessage(" first "))
	child := session.New()
	child.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleSystem, Content: "hidden"}))
	child.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: " nested\nanswer "}))
	grandchild := session.New()
	grandchild.AddMessage(session.UserMessage("grandchild"))
	child.AddSubSession(grandchild)
	sess.AddSubSession(child)
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: " \t\n "}))
	sess.AddMessage(session.UserMessage(" last "))
	sess.AddMessage(session.UserMessage(""))
	for _, limit := range []int{-1, 0, 1, 2, 3, 4, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			require.Equal(t, renderTranscriptFullHistory(sess, limit), renderTranscript(sess, limit))
		})
	}
	require.Equal(t, "user: last", renderTranscript(sess, 1))
	require.Empty(t, renderTranscript(session.New(), 2))
}

func BenchmarkRenderTranscriptTail(b *testing.B) {
	for _, count := range []int{100, 10000} {
		sess := session.New()
		for range count {
			sess.AddMessage(session.UserMessage(strings.Repeat("message ", 32)))
		}
		for _, impl := range []struct {
			name   string
			render func(*session.Session, int) string
		}{{"full_format_baseline", renderTranscriptFullHistory}, {"tail_format", renderTranscript}} {
			b.Run(fmt.Sprintf("%d/%s", count, impl.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					impl.render(sess, 5)
				}
			})
		}
	}
}
