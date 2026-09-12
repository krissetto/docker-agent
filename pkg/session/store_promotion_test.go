package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestStoresPromotePendingPreserveLaterSummaryKeptTail(t *testing.T) {
	factories := map[string]func(*testing.T) Store{
		"memory": func(t *testing.T) Store {
			t.Helper()
			return NewInMemorySessionStore()
		},
		"sqlite": func(t *testing.T) Store {
			t.Helper()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			return store
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			store := factory(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			sess := New(WithID("kept-tail-" + name))
			require.NoError(t, store.AddSession(t.Context(), sess))
			_, err := store.AddMessage(t.Context(), sess.ID, UserMessage("older history"))
			require.NoError(t, err)
			pendingA := UserMessage("new turn A")
			pendingA.Pending, pendingA.Accepted, pendingA.TurnID = true, true, "turn-a"
			_, err = store.AddMessage(t.Context(), sess.ID, pendingA)
			require.NoError(t, err)
			pendingB := UserMessage("new turn B")
			pendingB.Pending, pendingB.Accepted, pendingB.TurnID = true, true, "turn-b"
			_, err = store.AddMessage(t.Context(), sess.ID, pendingB)
			require.NoError(t, err)
			_, err = store.AddMessage(t.Context(), sess.ID, &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "kept assistant"}})
			require.NoError(t, err)
			require.NoError(t, store.AddSummary(t.Context(), sess.ID, Item{Summary: "older summary", FirstKeptEntry: 3}))

			require.NoError(t, store.PromotePendingUserMessage(t.Context(), sess.ID, "turn-a"))
			require.NoError(t, store.PromotePendingUserMessage(t.Context(), sess.ID, "turn-b"))
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			items := loaded.MessagesSnapshot()
			require.Len(t, items, 5)
			assert.Equal(t, 1, items[2].FirstKeptEntry)
			messages := loaded.GetMessages(agent.New("root", ""))
			require.Len(t, messages, 4)
			assert.Contains(t, messages[0].Content, "Session Summary: older summary")
			assert.Equal(t, "kept assistant", messages[1].Content)
			assert.Equal(t, "new turn A", messages[2].Content)
			assert.Equal(t, "new turn B", messages[3].Content)
		})
	}
}
