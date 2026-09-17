package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestItemBatchAtomicityConflictAndEmptyReceipt(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			batch := store.(ItemBatchAppender)
			items := []Item{NewMessageItem(UserMessage("one")), NewMessageItem(UserMessage("two"))}
			invalid := append(append([]Item(nil), items...), Item{})
			_, err := batch.AppendItems(t.Context(), root.ID, "invalid", invalid)
			require.Error(t, err)
			unchanged, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Empty(t, unchanged.MessagesSnapshot())
			first, err := batch.AppendItems(t.Context(), root.ID, "batch", items)
			require.NoError(t, err)
			require.Len(t, first.IDs, 2)
			retry, err := batch.AppendItems(t.Context(), root.ID, "batch", items)
			require.NoError(t, err)
			assert.True(t, retry.Duplicate)
			assert.Equal(t, first.IDs, retry.IDs)
			_, err = batch.AppendItems(t.Context(), root.ID, "batch", items[:1])
			require.ErrorIs(t, err, ErrWriteConflict)
			_, err = batch.AppendItems(t.Context(), root.ID, "empty", nil)
			require.NoError(t, err)
			empty, err := batch.AppendItems(t.Context(), root.ID, "empty", nil)
			require.NoError(t, err)
			assert.True(t, empty.Duplicate)
		})
	}
}

func TestItemBatchModelChangeSharesAtomicCommit(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			sess := New(WithAgentName("root"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			batch := store.(ItemBatchModelAppender)
			_, err := batch.AppendItemsWithModel(t.Context(), sess.ID, "invalid", []Item{{}}, "root", "new/model")
			require.Error(t, err)
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Empty(t, loaded.AgentModelOverrides)
			items := []Item{NewMessageItem(UserMessage("input"))}
			receipt, err := batch.AppendItemsWithModel(t.Context(), sess.ID, "valid", items, "root", "new/model")
			require.NoError(t, err)
			assert.False(t, receipt.Duplicate)
			loaded, err = store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, "new/model", loaded.AgentModelOverrides["root"])
			require.Len(t, loaded.MessagesSnapshot(), 1)
		})
	}
}
