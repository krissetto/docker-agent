package message

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func genericNotice(body string) *types.Message {
	input := session.UserMessage(body)
	input.InputOrigin, input.InputMode = session.InputOriginRuntime, "steer"
	return types.Input(input)
}

func TestGenericRuntimeNoticesExpandWithEventLabel(t *testing.T) {
	shell := "Shell command job_7 with status completed (exit code 0).\nCommand: make test\n\nCompletion output is delivered automatically and is not retained after delivery.\n\n--- Untrusted command output ---\nok\tpkg\t0.1s\n"
	for _, tc := range []struct {
		name, body, label string
		detail            []string
	}{
		{"shell", shell, "Background shell completed", []string{"Command: make test", "ok    pkg    0.1s"}},
		{"webhook", "\u26a0\ufe0f Delivery to the slack webhook failed permanently: HTTP 404", "Webhook delivery failed", []string{"failed permanently: HTTP 404"}},
		{"schedule", "\u23f0 Scheduled task \"nightly\" (s1) is due:\nrun the audit", "Scheduled task due", []string{"run the audit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := genericNotice(tc.body)
			view := New(animation.NewRuntime(), msg, nil)
			view.SetSize(90, 100)

			collapsed := ansi.Strip(view.View())
			assert.Contains(t, collapsed, tc.label+" >")
			assert.NotContains(t, collapsed, "From ", "sender-less notices carry no sender header")
			assert.NotContains(t, collapsed, "Runtime update received")
			for _, detail := range tc.detail {
				assert.NotContains(t, collapsed, detail)
			}
			assert.True(t, view.IsToggleAt(0, 0), "the event header is the disclosure target")
			_, navigable := view.InputReferenceForLine(0)
			assert.False(t, navigable, "no sender to navigate to")

			view.SetExpanded(true)
			expanded := ansi.Strip(view.View())
			assert.Contains(t, expanded, tc.label+" v")
			for _, detail := range tc.detail {
				assert.Contains(t, expanded, detail)
			}
			assert.NotContains(t, expanded, "system_info")
			assert.Less(t, len(strings.Split(collapsed, "\n")), len(strings.Split(expanded, "\n")))

			view.SetExpanded(false)
			assert.NotContains(t, ansi.Strip(view.View()), tc.detail[0], "collapse hides the detail again")
		})
	}
}

func TestUnclassifiedGenericRuntimeNoticeStaysUnexpandable(t *testing.T) {
	msg := genericNotice("<system_info>private model envelope</system_info>")
	assert.Empty(t, msg.ReceivedBody)
	view := New(animation.NewRuntime(), msg, nil)
	view.SetExpanded(true)
	out := ansi.Strip(view.View())
	assert.Contains(t, out, "Runtime update received")
	assert.NotContains(t, out, "private model")
	assert.NotContains(t, out, ">")
	assert.False(t, view.IsToggleAt(0, 0))
}

func TestGenericRuntimeNoticeHeaderWrapsWithinWidth(t *testing.T) {
	msg := genericNotice("\u23f0 Scheduled task \"a very long schedule name for narrow terminals\" (s9) is due:\nbody")
	view := New(animation.NewRuntime(), msg, nil)
	for _, width := range []int{8, 20, 40} {
		view.SetSize(width, 100)
		view.InvalidateRenderCache()
		for line := range strings.SplitSeq(ansi.Strip(view.View()), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width)
		}
	}
}
