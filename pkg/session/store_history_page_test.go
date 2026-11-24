package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteViewHistoryPagesAfterSparseWrites(t *testing.T) {
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "history.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db := store.(*SQLiteSessionStore)
	root := New()
	root.AddMessage(UserMessage("first"))
	queued := UserMessage("queued")
	queued.Pending, queued.Accepted, queued.TurnID = true, true, "pending"
	root.AddMessage(queued)
	root.AddMessage(UserMessage(strings.Repeat("large", 1000)))
	root.AddMessage(UserMessage("fourth"))
	require.NoError(t, db.AddSession(t.Context(), root))
	require.NoError(t, db.PromotePendingUserMessage(t.Context(), root.ID, queued.TurnID))
	child := New(WithParentID(root.ID))
	require.NoError(t, db.AddSubSession(t.Context(), root.ID, child))
	require.NoError(t, db.DeleteSession(t.Context(), child.ID))
	_, err = db.AddMessage(t.Context(), root.ID, queued)
	require.NoError(t, err)
	_, err = db.AddMessage(t.Context(), root.ID, UserMessage("last"))
	require.NoError(t, err)

	total, pending, err := db.ReadViewHistoryState(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, 6, total)
	require.Len(t, pending, 1)
	assert.Equal(t, 4, pending[0].Position)
	before, key := total, int64(0)
	var positions []int
	for before > 0 {
		page, err := db.ReadViewHistoryPage(t.Context(), root.ID, total, before, 2, 1024, key, func(BoundedItemPage) bool { return true })
		require.NoError(t, err)
		require.Equal(t, before, page.BeforePosition)
		require.Equal(t, total, page.TotalItems)
		require.Equal(t, min(2, before), page.ScannedPositions)
		if before == 2 {
			assert.Equal(t, []int{1}, page.OmittedPositions)
		}
		for _, item := range page.Items {
			positions = append(positions, item.Position)
		}
		before, key = page.NextBeforePosition, page.NextStoragePosition
	}
	assert.Equal(t, []int{4, 5, 2, 3, 0}, positions)
}

func TestSQLiteViewHistoryRecoversSmallOpenStream(t *testing.T) {
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "stream.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db := store.(*SQLiteSessionStore)
	root := New()
	require.NoError(t, db.AddSession(t.Context(), root))
	_, err = db.AppendMessageChunks(t.Context(), root.ID, &StreamStart{WriteID: "open", AgentName: "root", MessageID: "stream"}, 0, 0, []MessageChunk{{Kind: MessageChunkContent, Text: "partial"}})
	require.NoError(t, err)
	page, err := db.ReadViewHistoryPage(t.Context(), root.ID, 1, 1, 1, 1024, 0, func(BoundedItemPage) bool { return true })
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "partial", page.Items[0].Item.Message.Message.Content)
	assert.Equal(t, "stream", page.Items[0].Item.Message.Message.MessageID)
}
