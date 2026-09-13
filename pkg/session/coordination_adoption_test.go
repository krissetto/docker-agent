package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestCoordinationAdoptsLegacyRowsWithoutRewritingHistory(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			legacy := New(WithParentID(root.ID), WithUserMessage("legacy input"), WithAgentName("active-handoff"), WithAttachedFiles([]string{"/tmp/legacy.txt"}), WithAttributes(map[string]string{"docker-agent.actor.agent": "creation-agent"}))
			nested := New(WithUserMessage("nested history"))
			legacy.AddSubSession(nested)
			require.NoError(t, store.AddSubSession(t.Context(), root.ID, legacy))
			original, err := store.GetSession(t.Context(), legacy.ID)
			require.NoError(t, err)
			stale := original.Clone()
			stale.AttachedFiles = nil
			stale.AgentName = "stale-agent"
			stale.Messages[0].Message.Message.Content = "stale input"
			_, err = store.AddMessage(t.Context(), legacy.ID, UserMessage("newer result"))
			require.NoError(t, err)
			cs := store.(CoordinationStore)
			record := ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "legacy-node", SessionID: legacy.ID, Agent: "creation-agent"}}
			admission := ChildAdmission{Child: stale, Record: record}
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			records, err := cs.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, uint64(1), records[0].Revision)
			loaded, err := store.GetSession(t.Context(), legacy.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 3)
			assert.Equal(t, original.Messages[0].Message.ID, loaded.Messages[0].Message.ID)
			assert.Equal(t, "legacy input", loaded.Messages[0].Message.Message.Content)
			assert.Equal(t, "nested history", loaded.Messages[1].SubSession.Messages[0].Message.Message.Content)
			assert.Equal(t, "newer result", loaded.Messages[2].Message.Message.Content)
			assert.Equal(t, []string{"/tmp/legacy.txt"}, loaded.AttachedFiles)
			assert.Equal(t, "active-handoff", loaded.AgentName)
			parent, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, parent.Messages, 1)
			record.Result = "canonical result"
			require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: record}))
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			records, err = cs.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Equal(t, uint64(2), records[0].Revision)
			assert.Equal(t, "canonical result", records[0].Result)
		})
	}
}

func TestCoordinationLegacyAdoptionRejectsMismatchedBindings(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			root, other := New(), New()
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), other))
			legacy := New(WithParentID(root.ID), WithUserMessage("legacy"), WithAttributes(map[string]string{"docker-agent.actor.agent": "worker"}))
			require.NoError(t, store.AddSubSession(t.Context(), root.ID, legacy))
			cs := store.(CoordinationStore)
			record := ChildRecord{RootSessionID: root.ID, ParentSessionID: root.ID, Node: subagent.Node{ID: "legacy", SessionID: legacy.ID, Agent: "worker"}}
			wrong := record
			wrong.ParentSessionID = other.ID
			require.ErrorIs(t, cs.AdmitChild(t.Context(), ChildAdmission{Child: legacy, Record: wrong}), ErrAlreadyExists)
			wrong = record
			wrong.RootSessionID = other.ID
			require.ErrorIs(t, cs.AdmitChild(t.Context(), ChildAdmission{Child: legacy, Record: wrong}), ErrAlreadyExists)
			wrong = record
			wrong.Node.Agent = "different-agent"
			require.ErrorIs(t, cs.AdmitChild(t.Context(), ChildAdmission{Child: legacy, Record: wrong}), ErrAlreadyExists)
			require.NoError(t, cs.AdmitChild(t.Context(), ChildAdmission{Child: legacy, Record: record}))
			collision := coordinationAdmission(root.ID)
			collision.Record.Node.ID = record.Node.ID
			require.ErrorIs(t, cs.AdmitChild(t.Context(), collision), ErrAlreadyExists)
		})
	}
}
