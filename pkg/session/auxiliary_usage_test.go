package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
)

func TestAuxiliaryUsageMetadataRoundTrip(t *testing.T) {
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			sess := New()
			record := AuxiliaryUsage{Agent: "root", Model: "test/model", Cost: 0.25, Usage: chat.Usage{InputTokens: 40, OutputTokens: 2, ReasoningTokens: 1}}
			sess.AddAuxiliaryUsage(record)
			sess.AddAuxiliaryUsage(record)
			sess.AddMessage(UserMessage("real input"))
			require.NoError(t, store.AddSession(t.Context(), sess))
			stored, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Len(t, stored.AuxiliaryUsageSnapshot(), 1)
			assert.Equal(t, int64(80), stored.AuxiliaryUsageSnapshot()[0].Usage.InputTokens)
			assert.Equal(t, int64(2), stored.AuxiliaryUsageSnapshot()[0].Usage.ReasoningTokens)
			assert.Equal(t, 0.5, stored.TotalCost())
			assert.Equal(t, 0.5, stored.OwnCost())
			assert.Len(t, stored.MessagesSnapshot(), 1)
			a := agent.New("root", "system")
			assert.Len(t, stored.GetMessages(a), 2)
			data, err := json.Marshal(stored)
			require.NoError(t, err)
			var imported Session
			require.NoError(t, json.Unmarshal(data, &imported))
			assert.Equal(t, stored.AuxiliaryUsageSnapshot(), imported.AuxiliaryUsageSnapshot())
			imported.SetTitle("normal metadata save")
			require.NoError(t, store.UpdateSession(t.Context(), &imported))
			saved, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, 0.5, saved.TotalCost())
			assert.Len(t, saved.MessagesSnapshot(), 1)
		})
	}
}
