package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeNoticeLabelNamesProducedEvents(t *testing.T) {
	for _, tc := range []struct {
		name, body, label string
		ok                bool
	}{
		{"shell completed", "Shell command job_1 with status completed (exit code 0).\nCommand: make\n\n--- Untrusted command output ---\nok", "Background shell completed", true},
		{"shell failed", "Shell command job_2 with status failed (exit code 2).\n", "Background shell failed", true},
		{"shell without status", "Shell command job_3 finished oddly", "Background shell finished", true},
		{"shell empty status", "Shell command job_4 with status  (exit code 0).", "Background shell finished", true},
		{"shell multiline status", "Shell command job_5 with status a\nb (exit code 0).", "Background shell finished", true},
		{"webhook failure", "\u26a0\ufe0f Delivery to the slack webhook failed permanently: 404", "Webhook delivery failed", true},
		{"scheduled task", "\u23f0 Scheduled task \"nightly\" (s1) is due:\nrun the audit", "Scheduled task due", true},
		{"surrounding space", "  \n\u23f0 Scheduled task s2 is due:\nx\n", "Scheduled task due", true},
		{"framed envelope", "<system_info>Shell command job with status completed (exit code 0).</system_info>", "", false},
		{"plain text", "unchanged body", "", false},
		{"empty", "", "", false},
		{"lookalike subagent report", `Subagent "w" (id) finished its turn.`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			label, ok := RuntimeNoticeLabel(tc.body)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.label, label)
		})
	}
}
