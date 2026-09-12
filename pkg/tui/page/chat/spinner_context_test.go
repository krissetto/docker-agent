package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
)

// TestPendingSpinnerContext verifies the waiting-spinner label is scoped to
// delegated streams: depth < 2 keeps the default playful spinner (empty
// sender/label), while nested streams name the nearest parent → child pair and
// expose the child as the accent-color sender.
func streams(names []string) []lifecycle.Stream {
	result := make([]lifecycle.Stream, len(names))
	for i, name := range names {
		result[i].AgentName = name
	}
	return result
}

func TestPendingSpinnerContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stack      []string
		wantSender string
		wantLabel  string
	}{
		{"depth 0 - no stream", nil, "", ""},
		{"depth 1 - top-level turn", []string{"root"}, "", ""},
		{"depth 2 - single delegation", []string{"root", "researcher"}, "researcher", "root → researcher"},
		{"depth 3 - nested delegation", []string{"root", "researcher", "librarian"}, "librarian", "researcher → librarian"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &chatPage{lifecycle: lifecycle.State{Streams: streams(tc.stack)}}
			sender, label := p.pendingSpinnerContext()

			assert.Equal(t, tc.wantSender, sender, "sender (accent agent)")
			assert.Equal(t, tc.wantLabel, label, "parent → child label")
		})
	}
}
