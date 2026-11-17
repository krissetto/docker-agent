package lifecycle

import "strings"

const (
	shellCompletionPrefix = "Shell command "
	shellStatusMarker     = " with status "
	shellExitMarker       = " (exit code "
	webhookFailurePrefix  = "\u26a0\ufe0f Delivery to the "
	scheduledTaskPrefix   = "\u23f0 Scheduled task "
)

// RuntimeNoticeLabel names the runtime event behind a sender-less notice. The
// producers (shell job completion, webhook failure recall, scheduler fire) emit
// fixed tool-authored bodies, so the body is the only durable evidence after a
// reload. ok is false for any other body, which must stay unexpandable.
func RuntimeNoticeLabel(body string) (label string, ok bool) {
	body = strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(body, shellCompletionPrefix):
		rest, found := strings.CutPrefix(body, shellCompletionPrefix)
		if !found {
			return "", false
		}
		_, rest, found = strings.Cut(rest, shellStatusMarker)
		if !found {
			return "Background shell finished", true
		}
		status, _, found := strings.Cut(rest, shellExitMarker)
		if !found || status == "" || strings.ContainsAny(status, "\r\n") {
			return "Background shell finished", true
		}
		return "Background shell " + status, true
	case strings.HasPrefix(body, webhookFailurePrefix):
		return "Webhook delivery failed", true
	case strings.HasPrefix(body, scheduledTaskPrefix):
		return "Scheduled task due", true
	}
	return "", false
}
