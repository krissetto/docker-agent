package streamcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveKeepsRuntimeMessageID(t *testing.T) {
	t.Parallel()

	var tr Tracker
	assert.Equal(t, Identity{SessionID: "s", MessageID: "answer"}, tr.Resolve("s", "answer"))
	assert.Equal(t, Identity{SessionID: "s", MessageID: "answer"}, tr.Resolve("s", "answer"),
		"a runtime ID is returned unchanged and never replaced by a legacy one")
}

func TestResolveLegacyIsStableUntilFinish(t *testing.T) {
	t.Parallel()

	var tr Tracker
	first := tr.Resolve("s", "")
	require.Equal(t, "legacy:1", first.MessageID)
	assert.Equal(t, first, tr.Resolve("s", ""), "chunks of one stream share one legacy identity")

	tr.Finish("s")
	second := tr.Resolve("s", "")
	assert.Equal(t, "legacy:2", second.MessageID, "a finished stream starts a new logical message")
}

func TestResolveLegacyIsPerSession(t *testing.T) {
	t.Parallel()

	var tr Tracker
	a := tr.Resolve("a", "")
	b := tr.Resolve("b", "")
	assert.NotEqual(t, a.MessageID, b.MessageID)
	tr.Finish("a")
	assert.Equal(t, b, tr.Resolve("b", ""), "finishing one session leaves another session's stream open")
}
