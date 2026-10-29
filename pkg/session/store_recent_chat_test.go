package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestRecentChatReadsBoundedVisibleChronologicalWindow(t *testing.T) {
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			root := New(WithID("recent"), WithAttributes(map[string]string{"docker-agent.actor.agent": "root"}))
			root.AddMessage(UserMessage("old"))
			root.AddMessage(NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "visible", ReasoningContent: "hidden reasoning", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "hidden reasoning"}, {Type: chat.AssistantPartContent, Text: "visible answer"}}}))
			root.AddMessage(ImplicitUserMessage("implicit"))
			root.AddMessage(UserMessage("pending"))
			root.Messages[3].Message.Pending = true
			root.AddMessage(SystemMessage("system"))
			root.AddMessage(UserMessage(strings.Repeat("é", 12)))
			require.NoError(t, store.AddSession(t.Context(), root))
			reader, ok := store.(RecentChatStore)
			require.True(t, ok)
			page, err := reader.ReadRecentChatItems(t.Context(), root.ID, 0, 0, 2, 16, 32)
			require.NoError(t, err)
			require.Len(t, page.Messages, 2)
			assert.Equal(t, "visible answer", page.Messages[1].Text)
			assert.False(t, page.Messages[0].Truncated)
			assert.LessOrEqual(t, len(page.Messages[0].Text), 32)
			assert.True(t, page.HasMore)
			older, err := reader.ReadRecentChatItems(t.Context(), root.ID, page.NextBeforePosition, page.NextBeforeItemID, 10, 16, 16)
			require.NoError(t, err)
			require.Len(t, older.Messages, 1)
			assert.Equal(t, "old", older.Messages[0].Text)
			assert.False(t, older.HasMore)
			assert.NotContains(t, page.Messages[1].Text, "reasoning")
		})
	}
}

func TestSQLiteRecentChatBoundsExtractionAndUsesSessionPositionIndex(t *testing.T) {
	store := openMemoryStore(t)
	root := New(WithID("recent-big"), WithAttributes(map[string]string{"docker-agent.actor.agent": "root"}))
	root.AddMessage(UserMessage(strings.Repeat("z", 2*1024*1024)))
	require.NoError(t, store.AddSession(t.Context(), root))
	var reader RecentChatStore = store
	page, err := reader.ReadRecentChatItems(t.Context(), root.ID, 0, 0, 1, 8, 32)
	require.NoError(t, err)
	require.Len(t, page.Messages, 1)
	assert.Len(t, page.Messages[0].Text, 32)
	assert.True(t, page.Messages[0].Truncated)
	assert.False(t, page.HasMore, "the only stored message is the whole page")
	rows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT id, position FROM session_items WHERE session_id = ? ORDER BY position DESC LIMIT ?`, root.ID, 8)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(details, " "), "idx_session_items_session")
}

func TestRecentChatScanLimitStopsHiddenHistoryWalk(t *testing.T) {
	store := NewInMemorySessionStore()
	root := New(WithID("recent-scan"))
	root.AddMessage(UserMessage("old"))
	for range 600 {
		root.AddMessage(SystemMessage("hidden"))
	}
	require.NoError(t, store.AddSession(t.Context(), root))
	page, err := store.(RecentChatStore).ReadRecentChatItems(t.Context(), root.ID, 0, 0, 1, 64, 32)
	require.NoError(t, err)
	assert.Empty(t, page.Messages)
	assert.True(t, page.HasMore)
	assert.Equal(t, 537, page.NextBeforePosition)
}

func TestRecentChatMetadataIsBounded(t *testing.T) {
	store := openMemoryStore(t)
	root := New(WithID("recent-metadata"), WithAttributes(map[string]string{"docker-agent.actor.agent": "root"}))
	require.NoError(t, store.AddSession(t.Context(), root))
	_, err := store.db.ExecContext(t.Context(), `UPDATE sessions SET attributes = ? WHERE id = ?`, `{"docker-agent.actor.agent":"root","docker-agent.actor.source":"`+strings.Repeat("s", recentChatAttributesLimit)+`"}`, root.ID)
	require.NoError(t, err)
	var reader RecentChatStore = store
	_, err = reader.ReadRecentChatMetadata(t.Context(), root.ID)
	require.ErrorIs(t, err, ErrRecentChatMetadataTooLarge)
}
