package openai

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseDiagnosticLogsRedactOpaqueState(t *testing.T) {
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	events := decodeEvents(t, []map[string]any{
		{"type": "response.future", "response": map[string]any{"encrypted_content": "future-secret", "nested": []any{map[string]any{"encrypted_content": "nested-secret"}}}},
		{"type": "response.incomplete", "response": map[string]any{"id": "partial", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{map[string]any{"type": "reasoning", "encrypted_content": "partial-secret"}}}},
	})
	adapter := responseStateClient().responseAdapter(t.Context(), &fakeEventStream{events: events})
	for range 2 {
		_, err := adapter.Recv()
		require.NoError(t, err)
	}
	for _, secret := range []string{"future-secret", "nested-secret", "partial-secret"} {
		assert.NotContains(t, log.String(), secret)
	}
	assert.Contains(t, log.String(), "redacted")
	assert.Contains(t, log.String(), "max_output_tokens")
}
