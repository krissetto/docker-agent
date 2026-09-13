package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func editablePendingMessage(turnID string) *Message {
	msg := UserMessage("old", chat.MessagePart{Type: chat.MessagePartTypeText, Text: "old\n\nattachment"}, chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "image", Source: chat.DocumentSource{InlineData: []byte{1, 2, 3}}}})
	msg.Pending, msg.Accepted, msg.TurnID, msg.InputOrigin = true, true, turnID, InputOriginUser
	msg.InputMode, msg.SenderID, msg.SenderName = "steer", "sender", "name"
	return msg
}

func TestPendingEditStoresPreserveAdmission(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			store := NewInMemorySessionStore()
			path := filepath.Join(t.TempDir(), "edit.db")
			if kind == "sqlite" {
				var err error
				store, err = newSQLiteStoreForTest(t, path)
				require.NoError(t, err)
			}
			sess := New(WithID("edit"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			for _, id := range []string{"first", "target", "last"} {
				_, err := store.AddMessage(t.Context(), sess.ID, editablePendingMessage(id))
				require.NoError(t, err)
			}
			before, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			items := before.MessagesSnapshot()
			editor := store.(PendingMessageEditor)
			require.ErrorIs(t, editor.EditPendingUserMessage(t.Context(), "wrong", "target", "new"), ErrPendingMessageStale)
			require.NoError(t, editor.EditPendingUserMessage(t.Context(), sess.ID, "target", "new"))
			want := items
			want[1].Message.Message.Content = "new"
			want[1].Message.Message.MultiContent[0].Text = "new\n\nattachment"
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, want, loaded.MessagesSnapshot())
			require.NoError(t, store.Close())
			if kind == "sqlite" {
				for range 2 {
					store, err = newSQLiteStoreForTest(t, path)
					require.NoError(t, err)
					loaded, err = store.GetSession(t.Context(), sess.ID)
					require.NoError(t, err)
					assert.Equal(t, want, loaded.MessagesSnapshot())
					require.NoError(t, store.Close())
				}
			}
		})
	}
}

func TestPendingEditDetachedLayout(t *testing.T) {
	original := editablePendingMessage("turn")
	next, err := PendingUserMessageReplacement(original, "new")
	require.NoError(t, err)
	next.Message.MultiContent[1].Document.Source.InlineData[0] = 9
	assert.Equal(t, byte(1), original.Message.MultiContent[1].Document.Source.InlineData[0])
	assert.Equal(t, "old\n\nattachment", original.Message.MultiContent[0].Text)
	for _, shape := range []string{"prefix", "extra_text", "nonhuman", "consumed", "unaccepted", "empty"} {
		t.Run(shape, func(t *testing.T) {
			msg := editablePendingMessage("turn")
			content := "new"
			switch shape {
			case "prefix":
				msg.Message.MultiContent[0].Text = "unrelated"
			case "extra_text":
				msg.Message.MultiContent = append(msg.Message.MultiContent, chat.MessagePart{Type: chat.MessagePartTypeText, Text: "ambiguous"})
			case "nonhuman":
				msg.InputOrigin = InputOriginAgent
			case "consumed":
				msg.Pending = false
			case "unaccepted":
				msg.Accepted = false
			case "empty":
				msg.Message.MultiContent, content = nil, "  "
			}
			before := cloneMessage(msg)
			_, err := PendingUserMessageReplacement(msg, content)
			require.Error(t, err)
			assert.Equal(t, before, msg)
		})
	}
}

func TestPendingEditSQLiteRollbackAndZeroEligible(t *testing.T) {
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "edit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := New(WithID("rollback"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	_, err = store.AddMessage(t.Context(), sess.ID, editablePendingMessage("turn"))
	require.NoError(t, err)
	sqlite := store.(*SQLiteSessionStore)
	for _, action := range []string{"RAISE(ABORT, 'edit failed')", "RAISE(IGNORE)"} {
		_, err = sqlite.db.ExecContext(t.Context(), `CREATE TRIGGER fail_edit BEFORE UPDATE OF message_json ON session_items BEGIN SELECT `+action+`; END`)
		require.NoError(t, err)
		require.Error(t, sqlite.EditPendingUserMessage(t.Context(), sess.ID, "turn", "new"))
		loaded, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		assert.Equal(t, "old", loaded.MessagesSnapshot()[0].Message.Message.Content)
		_, err = sqlite.db.ExecContext(t.Context(), `DROP TRIGGER fail_edit`)
		require.NoError(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, sqlite.EditPendingUserMessage(ctx, sess.ID, "turn", "new"), context.Canceled)
	require.NoError(t, store.PromotePendingUserMessage(t.Context(), sess.ID, "turn"))
	require.ErrorIs(t, sqlite.EditPendingUserMessage(t.Context(), sess.ID, "turn", "new"), ErrPendingMessageStale)
}

func TestPendingEditStoreRejectsIneligibleAndAmbiguousIdentity(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, invalid := range []string{"nonhuman", "unset_origin", "consumed_prefix", "duplicate_pending", "layout"} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				store := NewInMemorySessionStore()
				if kind == "sqlite" {
					var err error
					store, err = newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "edit.db"))
					require.NoError(t, err)
				}
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				sess := New(WithID("s"))
				require.NoError(t, store.AddSession(t.Context(), sess))
				msg := editablePendingMessage("turn")
				switch invalid {
				case "nonhuman":
					msg.InputOrigin = InputOriginAgent
				case "unset_origin":
					msg.InputOrigin = ""
				case "consumed_prefix", "duplicate_pending":
					prefix := editablePendingMessage("turn")
					prefix.Pending = invalid == "duplicate_pending"
					_, err := store.AddMessage(t.Context(), sess.ID, prefix)
					require.NoError(t, err)
				case "layout":
					msg.Message.MultiContent[0].Text = "unrelated"
				}
				_, err := store.AddMessage(t.Context(), sess.ID, msg)
				require.NoError(t, err)
				before, err := store.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				require.Error(t, store.(PendingMessageEditor).EditPendingUserMessage(t.Context(), sess.ID, "turn", "new"))
				after, err := store.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				assert.Equal(t, before.MessagesSnapshot(), after.MessagesSnapshot())
			})
		}
	}
}

func TestPendingEditSharedMemorySlotIsIdempotent(t *testing.T) {
	store := NewInMemorySessionStore().(*InMemorySessionStore)
	sess := New(WithID("shared"))
	sess.AddMessage(editablePendingMessage("turn"))
	store.sessions.Store(sess.ID, sess)
	replacement, err := PendingUserMessageReplacement(sess.MessagesSnapshot()[0].Message, "new")
	require.NoError(t, err)
	require.NoError(t, store.EditPendingUserMessage(t.Context(), sess.ID, "turn", "new"))
	require.True(t, sess.ReplacePendingUserMessagePayload("turn", replacement.Message.Content, replacement.Message.MultiContent))
	assert.Equal(t, "new\n\nattachment", sess.MessagesSnapshot()[0].Message.Message.MultiContent[0].Text)
	assert.Equal(t, 1, sess.ItemCount())
}
