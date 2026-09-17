package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestChildCommitBatchAtomicityAndStoppedMonotonicity(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			coord := store.(CoordinationStore)
			batch := store.(ChildCommitBatchStore)
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			for _, id := range []subagent.NodeID{"one", "two"} {
				admission := coordinationAdmission(root.ID)
				admission.Record.Node.ID = id
				require.NoError(t, coord.AdmitChild(t.Context(), admission))
			}
			records, err := coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			commits := make([]ChildCommit, 0, 2)
			for _, record := range records {
				record.Node.State = subagent.NodeStopped
				commits = append(commits, ChildCommit{ExpectedRevision: record.Revision, Record: record})
			}
			commits[1].ExpectedRevision++
			require.ErrorIs(t, batch.CommitChildren(t.Context(), commits), ErrRevisionConflict)
			after, err := coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			assert.ElementsMatch(t, records, after, "no partial subtree")
			commits[1].ExpectedRevision--
			require.NoError(t, batch.CommitChildren(t.Context(), commits))
			after, err = coord.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			for _, record := range after {
				assert.Equal(t, subagent.NodeStopped, record.Node.State)
				record.Node.State = subagent.NodeIdle
				require.Error(t, coord.CommitChild(t.Context(), ChildCommit{ExpectedRevision: record.Revision, Record: record}))
				record.Node.State = subagent.NodeStopped
				require.Error(t, coord.CommitChild(t.Context(), ChildCommit{ExpectedRevision: record.Revision, Record: record, Reports: []ChildReport{{ID: "new", TurnID: "turn", ChildSessionID: record.Node.SessionID, ParentSessionID: root.ID}}}))
			}
		})
	}
}
