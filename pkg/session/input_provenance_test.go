package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInputOriginAndSenderRoundTrip(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			for _, origin := range []InputOrigin{"", InputOriginUser, InputOriginAgent, InputOriginRuntime, "unknown"} {
				t.Run(string(origin), func(t *testing.T) {
					message := UserMessage("<system_info>literal body</system_info>")
					message.InputOrigin, message.SenderID, message.SenderName = origin, "source-session", "source-agent"
					message.InputMode = "runtime_note"
					data, err := json.Marshal(message)
					require.NoError(t, err)
					var decoded Message
					require.NoError(t, json.Unmarshal(data, &decoded))
					assert.Equal(t, message, &decoded)
					sess := New()
					sess.AddMessage(message)
					require.NoError(t, store.AddSession(t.Context(), sess))
					_, err = store.AddMessage(t.Context(), sess.ID, message)
					require.NoError(t, err)
					id, err := store.(ItemAppender).AppendItem(t.Context(), sess.ID, "append", Item{Message: message})
					require.NoError(t, err)
					loaded, err := store.GetSession(t.Context(), sess.ID)
					require.NoError(t, err)
					require.Len(t, loaded.Messages, 3)
					for _, item := range loaded.Messages {
						got := cloneMessage(item.Message)
						got.ID = 0
						assert.Equal(t, message, got)
					}
					updated := cloneMessage(message)
					updated.InputOrigin, updated.SenderID, updated.SenderName = InputOriginAgent, "other-session", "other-agent"
					updated.Message.Content = "clean agent body"
					require.NoError(t, store.UpdateMessage(t.Context(), sess.ID, id, updated))
					loaded, err = store.GetSession(t.Context(), sess.ID)
					require.NoError(t, err)
					updated.ID = id
					assert.Equal(t, updated, loaded.Messages[2].Message)
				})
			}
		})
	}
}

func TestItemAppenderRejectsProvenanceReuse(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			sess := New()
			require.NoError(t, store.AddSession(t.Context(), sess))
			original := UserMessage("same text")
			appender := store.(ItemAppender)
			id, err := appender.AppendItem(t.Context(), sess.ID, "write", Item{Message: original})
			require.NoError(t, err)
			for _, change := range []func(*Message){
				func(m *Message) { m.InputOrigin = InputOriginRuntime },
				func(m *Message) { m.SenderID = "source-session" },
				func(m *Message) { m.SenderName = "source-agent" },
			} {
				changed := cloneMessage(original)
				change(changed)
				_, err := appender.AppendItem(t.Context(), sess.ID, "write", Item{Message: changed})
				require.ErrorIs(t, err, ErrWriteConflict)
			}
			retryID, err := appender.AppendItem(t.Context(), sess.ID, "write", Item{Message: original})
			require.NoError(t, err)
			assert.Equal(t, id, retryID)
		})
	}
}

func TestSQLiteInputProvenanceReopens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "provenance.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	sess := New()
	message := UserMessage("clean body")
	message.InputOrigin, message.SenderID, message.SenderName = InputOriginAgent, "child-session", "maker"
	sess.AddMessage(message)
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	message.ID = loaded.Messages[0].Message.ID
	assert.Equal(t, message, loaded.Messages[0].Message)
}
