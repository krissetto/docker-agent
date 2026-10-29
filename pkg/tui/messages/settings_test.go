package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseVisibleReplies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  VisibleReplies
	}{
		{"latest", VisibleRepliesLatest},
		{"3", VisibleRepliesThree},
		{"5", VisibleRepliesFive},
		{"all", VisibleRepliesAll},
		{"", VisibleRepliesThree},
		{"invalid", VisibleRepliesThree},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			assert.Equal(t, tt.want, ParseVisibleReplies(tt.value))
		})
	}
}

func TestParseSendMode(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]SendMode{
		"":        SendModeSteer,
		"steer":   SendModeSteer,
		"queue":   SendModeQueue,
		"unknown": SendModeSteer,
	} {
		assert.Equal(t, want, ParseSendMode(raw), "busy_send_mode %q", raw)
	}
}
