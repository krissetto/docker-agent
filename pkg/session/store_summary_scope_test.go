package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedSessionSummariesIncludeChildrenWithoutChangingRootCatalog(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		name := "memory"
		if sqlite {
			name = "sqlite"
		}
		t.Run(name, func(t *testing.T) {
			store := NewInMemorySessionStore()
			if sqlite {
				store = openMemoryStore(t)
			}
			parent := New(WithID("parent"), WithUserMessage("parent transcript"))
			parent.CreatedAt = time.Unix(100, 0).UTC()
			child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("child transcript"), WithAttributes(map[string]string{"docker-agent.actor.agent": "worker"}))
			child.CreatedAt = time.Unix(200, 0).UTC()
			child.AgentModelOverrides = map[string]string{"worker": "provider/model"}
			require.NoError(t, store.AddSession(t.Context(), parent))
			require.NoError(t, store.AddSession(t.Context(), child))
			if sqlStore, ok := store.(*SQLiteSessionStore); ok {
				// Full hydration would fail, proving scope selection and metadata
				// extraction never parse transcript payloads for any included row.
				_, err := sqlStore.db.ExecContext(t.Context(), "UPDATE session_items SET message_json = 'not-json'")
				require.NoError(t, err)
			}
			roots, err := store.GetSessionSummaries(t.Context())
			require.NoError(t, err)
			require.Len(t, roots, 1)
			assert.Equal(t, parent.ID, roots[0].ID)
			scoped := store.(ScopedSummaryStore)
			rows, err := scoped.GetSessionSummariesWithScope(t.Context(), SummaryScope{IncludeChildren: true})
			require.NoError(t, err)
			require.Len(t, rows, 2)
			assert.Equal(t, child.ID, rows[0].ID)
			assert.Equal(t, parent.ID, rows[0].ParentID)
			assert.Equal(t, "worker", rows[0].Attributes["docker-agent.actor.agent"])
			assert.Equal(t, "provider/model", rows[0].AgentModelOverrides["worker"])
			assert.Equal(t, 1, rows[0].NumMessages)
			rows[0].AgentModelOverrides["worker"] = "mutated"
			rows[0].Attributes["docker-agent.actor.agent"] = "mutated"
			again, err := scoped.GetSessionSummariesWithScope(t.Context(), SummaryScope{IncludeChildren: true})
			require.NoError(t, err)
			assert.Equal(t, "provider/model", again[0].AgentModelOverrides["worker"])
			assert.Equal(t, "worker", again[0].Attributes["docker-agent.actor.agent"])
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = scoped.GetSessionSummariesWithScope(ctx, SummaryScope{IncludeChildren: true})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
