package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionMetadataReadsDoNotHydrateHistory(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			root := New(WithID("root"), WithAgentName("root"), WithWorkingDir("/workspace"), WithAttributes(map[string]string{"binding": "root"}))
			child := New(WithID("child"), WithParentID("root"))
			child.AddMessage(UserMessage("own history"))
			root.AddSubSession(child)
			require.NoError(t, store.AddSession(t.Context(), root))
			metadata, err := ReadSessionMetadata(t.Context(), store, root.ID)
			require.NoError(t, err)
			assert.Equal(t, root.ID, metadata.ID)
			assert.Equal(t, "/workspace", metadata.WorkingDir)
			assert.Equal(t, root.AttributesSnapshot(), metadata.AttributesSnapshot())
			assert.Empty(t, metadata.Messages)
			assert.Nil(t, metadata.GetSubagentTree())
			metadata.SetAttribute("binding", "mutated")
			again, err := ReadSessionMetadata(t.Context(), store, root.ID)
			require.NoError(t, err)
			assert.Equal(t, "root", again.AttributesSnapshot()["binding"])
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = ReadSessionMetadata(ctx, store, root.ID)
			require.ErrorIs(t, err, context.Canceled)
			_, err = ReadSessionMetadata(t.Context(), store, "missing")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}
