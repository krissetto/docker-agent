package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLegacyUnaddressedMessageQueueRejectsInput(t *testing.T) {
	q := NewInMemoryMessageQueue(3)
	assert.False(t, q.Enqueue(t.Context(), QueuedMessage{Content: "unaddressed"}))
	_, ok := q.Dequeue(t.Context())
	assert.False(t, ok)
	assert.Empty(t, q.Drain(t.Context()))
}
