package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteSnapshotPreservesActorMessageFields(t *testing.T) {
	t.Parallel()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root := New()
	child := New()
	message := UserMessage("accepted input")
	message.Pending, message.Accepted, message.TurnID = true, true, "turn-1"
	message.InputMode = "steer"
	root.AddMessage(message)
	child.AddMessage(message)
	root.AddSubSession(child)
	require.NoError(t, store.AddSession(t.Context(), root))
	loaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	for _, msg := range []*Message{loaded.Messages[0].Message, loaded.Messages[1].SubSession.Messages[0].Message} {
		assert.True(t, msg.Pending)
		assert.True(t, msg.Accepted)
		assert.Equal(t, "turn-1", msg.TurnID)
		assert.Equal(t, "steer", msg.InputMode)
		assert.NotZero(t, msg.ID)
	}
}

func TestSQLiteAncestorSnapshotDoesNotOverwriteDescendants(t *testing.T) {
	t.Parallel()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	root, child, grandchild := New(), New(), New(WithUserMessage("first"))
	child.AddSubSession(grandchild)
	root.AddSubSession(child)
	require.NoError(t, store.AddSession(t.Context(), root))
	_, err = store.AddMessage(t.Context(), grandchild.ID, UserMessage("newer"))
	require.NoError(t, err)
	require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
	loaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.Len(t, loaded.Messages[0].SubSession.Messages, 1)
	history := loaded.Messages[0].SubSession.Messages[0].SubSession.Messages
	require.Len(t, history, 2)
	assert.Equal(t, "newer", history[1].Message.Message.Content)
}

func TestOwnSnapshotReferencesDoNotCopyDescendantHistory(t *testing.T) {
	t.Parallel()
	root, child := New(WithUserMessage("root")), New(WithUserMessage("child"))
	root.AddSubSession(child)
	snapshot := root.OwnSnapshot()
	require.Len(t, snapshot.Messages, 2)
	assert.Equal(t, child.ID, snapshot.Messages[1].SubSession.ID)
	assert.Equal(t, root.ID, snapshot.Messages[1].SubSession.ParentID)
	assert.Empty(t, snapshot.Messages[1].SubSession.Messages)
	snapshot.Messages[0].Message.Message.Content = "changed"
	assert.Equal(t, "root", root.Messages[0].Message.Message.Content)
	assert.Equal(t, "child", child.Messages[0].Message.Message.Content)
}
