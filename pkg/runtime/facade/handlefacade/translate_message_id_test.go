package handlefacade

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
)

// Chunk identity is copied from the runtime and never synthesized.
func TestTranslateCarriesRuntimeMessageIDOnChunks(t *testing.T) {
	t.Parallel()
	tr := newTranslator("s")
	content := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: 1, Event: runtime.AgentChoice("root", "s", "hi", "m1")})
	require.Len(t, content, 1)
	assert.Equal(t, "m1", content[0].MessageID)
	reasoning := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: 2, Event: runtime.AgentChoiceReasoning("root", "s", "why", "m2")})
	require.Len(t, reasoning, 1)
	assert.Equal(t, "m2", reasoning[0].MessageID)
}

func TestTranslateLeavesMessageIDEmptyWhenNotProvided(t *testing.T) {
	t.Parallel()
	tr := newTranslator("s")
	chunk := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: 1, Event: runtime.AgentChoice("root", "s", "hi")})
	require.Len(t, chunk, 1)
	assert.Empty(t, chunk[0].MessageID, "a chunk without an ID is never given one")
	gap := tr.translate(runtime.SessionEvent{SessionID: "s", Gap: true, FirstAvailable: 5})
	require.Len(t, gap, 1)
	assert.Empty(t, gap[0].MessageID)
	settle := tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: 3, Event: runtime.StreamStopped("s", "root", "normal")})
	for _, ev := range settle {
		assert.Empty(t, ev.MessageID, "non-chunk events carry no message identity")
	}
}

// Ordering and sequence numbers are unchanged by the identity field.
func TestTranslatePreservesOrderAndSequenceWithIdentity(t *testing.T) {
	t.Parallel()
	tr := newTranslator("s")
	var got []facade.Event
	for i, event := range []runtime.Event{
		runtime.AgentChoice("root", "s", "a", "m1"),
		runtime.AgentChoice("root", "s", "b", "m1"),
		runtime.AgentChoiceReasoning("root", "s", "c", "m1"),
	} {
		got = append(got, tr.translate(runtime.SessionEvent{SessionID: "s", TurnID: "t", Sequence: uint64(i + 1), Event: event})...)
	}
	require.Len(t, got, 3)
	for i, ev := range got {
		assert.Equal(t, uint64(i+1), ev.Seq)
		assert.Equal(t, "m1", ev.MessageID)
	}
}
