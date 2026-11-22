package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestQueueSection_SingleMessage(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	m.SetQueuedMessages("Hello world")

	result := ansi.Strip(m.queueSection(40))

	// Should contain the title with count
	assert.Contains(t, result, "Queue (1)")

	// Should contain the message
	assert.Contains(t, result, "Hello world")

	// Should contain the clear hint
	assert.Contains(t, result, "Ctrl+X to clear")

	assert.Contains(t, result, "- Hello world")
}

func TestQueueSection_MultipleMessages(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	m.SetQueuedMessages("First", "Second", "Third")

	result := ansi.Strip(m.queueSection(40))

	// Should contain the title with count
	assert.Contains(t, result, "Queue (3)")

	// Should contain all messages
	assert.Contains(t, result, "First")
	assert.Contains(t, result, "Second")
	assert.Contains(t, result, "Third")

	// Should contain the clear hint
	assert.Contains(t, result, "Ctrl+X to clear")

	assert.Contains(t, result, "- First")
	assert.Contains(t, result, "- Third")
}

func TestQueueSection_LongMessageTruncation(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	// Create a very long message
	longMessage := strings.Repeat("x", 100)
	m.SetQueuedMessages(longMessage)

	result := m.queueSection(30) // Narrow width to force truncation

	// Should contain truncation indicator
	require.NotEmpty(t, result)

	// The full long message should not appear (it's truncated)
	assert.NotContains(t, result, longMessage)
}

func TestQueueSection_InRenderSections(t *testing.T) {
	t.Parallel()

	sessionState := &service.SessionState{}
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)
	m.SetSize(40, 100) // Set a reasonable size

	// Without queued messages, queue section should not appear in output
	linesWithoutQueue := m.renderSections(35)
	outputWithoutQueue := strings.Join(linesWithoutQueue, "\n")
	assert.NotContains(t, outputWithoutQueue, "Queue")

	// With queued messages, queue section should appear
	m.SetQueuedMessages("Pending task")
	linesWithQueue := m.renderSections(35)
	outputWithQueue := strings.Join(linesWithQueue, "\n")
	assert.Contains(t, outputWithQueue, "Queue (1)")
	assert.Contains(t, outputWithQueue, "Pending task")
}
