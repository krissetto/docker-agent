package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func coordinationAdmission(parent string) ChildAdmission {
	child := New(WithParentID(parent), WithAsyncSubagent(true))
	input := UserMessage("task")
	input.Pending, input.Accepted, input.TurnID = true, true, "initial-turn"
	child.AddMessage(input)
	return ChildAdmission{Child: child, Record: ChildRecord{
		RootSessionID: parent, ParentSessionID: parent,
		Node: subagent.Node{ID: "child", SessionID: child.ID, State: subagent.NodeStarting},
	}}
}

func TestCoordinationAdmissionCommitAndReportAcceptance(t *testing.T) {
	t.Parallel()
	for name, create := range map[string]func() Store{
		"memory": NewInMemorySessionStore,
		"sqlite": func() Store { return openMemoryStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			store := create()
			cs := store.(CoordinationStore)
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			admission := coordinationAdmission(root.ID)
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			records, err := cs.LoadChildren(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, uint64(1), records[0].Revision)
			child, err := store.GetSession(t.Context(), admission.Child.ID)
			require.NoError(t, err)
			require.Len(t, child.Messages, 1)
			assert.True(t, child.Messages[0].Message.Pending)
			assert.True(t, child.Messages[0].Message.Accepted)
			assert.Equal(t, "initial-turn", child.Messages[0].Message.TurnID)
			require.NotZero(t, child.Messages[0].Message.ID)
			require.NoError(t, store.PromotePendingUserMessage(t.Context(), child.ID, "initial-turn"))
			_, err = store.AddMessage(t.Context(), child.ID, UserMessage("result"))
			require.NoError(t, err)
			record := records[0]
			record.LastTurnID, record.Result, record.Node.State = "initial-turn", "result", subagent.NodeIdle
			report := ChildReport{ID: "report-1", ParentSessionID: root.ID, ChildSessionID: child.ID, TurnID: "initial-turn", Content: "result"}
			commit := ChildCommit{ExpectedRevision: 1, Record: record, Reports: []ChildReport{report}}
			require.NoError(t, cs.CommitChild(t.Context(), commit))
			require.ErrorIs(t, cs.CommitChild(t.Context(), commit), ErrRevisionConflict)
			pending, err := cs.PendingReports(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, uint64(2), pending[0].Revision)
			input := UserMessage("child finished")
			input.TurnID = "report-turn"
			input.InputMode = "runtime_note"
			id, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
			require.NoError(t, err)
			retryID, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
			require.NoError(t, err)
			require.Equal(t, id.MessageID, retryID.MessageID)
			assert.True(t, id.Created)
			assert.False(t, retryID.Created)
			pending, err = cs.PendingReports(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Empty(t, pending)
			loaded, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 2)
			require.Len(t, loaded.Messages[0].SubSession.Messages, 2)
			assert.False(t, loaded.Messages[0].SubSession.Messages[0].Message.Pending)
			assert.True(t, loaded.Messages[1].Message.Pending)
			assert.True(t, loaded.Messages[1].Message.Accepted)
			assert.Equal(t, id.MessageID, loaded.Messages[1].Message.ID)
			assert.Equal(t, "runtime_note", loaded.Messages[1].Message.InputMode)
			assert.Empty(t, root.Messages, "store must not append to a live owner")
		})
	}
}

func TestSQLiteCoordinationRestartAndAtomicFailures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "coordination.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	root := New()
	require.NoError(t, store.AddSession(t.Context(), root))
	admission := coordinationAdmission(root.ID)
	cs := store.(CoordinationStore)
	require.NoError(t, cs.AdmitChild(t.Context(), admission))
	admission.Record.Revision = 1
	report := ChildReport{ID: "report", ParentSessionID: root.ID, ChildSessionID: admission.Child.ID, TurnID: "initial-turn", Content: "done"}
	commit := ChildCommit{ExpectedRevision: 1, Record: admission.Record, Reports: []ChildReport{report}}
	require.NoError(t, cs.CommitChild(t.Context(), commit))
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cs = store.(CoordinationStore)
	pending, err := cs.PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	commit.ExpectedRevision = 2
	require.ErrorIs(t, cs.CommitChild(t.Context(), commit), ErrAlreadyExists)
	records, err := cs.LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), records[0].Revision, "failed report insert must roll back revision")
	_, err = cs.AcceptReport(t.Context(), "wrong-parent", report.ID, UserMessage("wrong"))
	require.ErrorIs(t, err, ErrNotFound)
	input := UserMessage("done")
	input.TurnID = "report-input"
	id, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	cs = store.(CoordinationStore)
	retryID, err := cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, id.MessageID, retryID.MessageID)
	assert.True(t, id.Created)
	assert.False(t, retryID.Created)
	var retained string
	require.NoError(t, store.(*SQLiteSessionStore).db.QueryRowContext(t.Context(), `SELECT content FROM child_reports WHERE id = ?`, report.ID).Scan(&retained))
	assert.Empty(t, retained, "acknowledged reports retain identity, not payload")
}

func TestCoordinationRejectedAdmissionAndCancellation(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			cs := store.(CoordinationStore)
			admission := coordinationAdmission("missing")
			require.ErrorIs(t, cs.AdmitChild(t.Context(), admission), ErrNotFound)
			_, err := store.GetSession(t.Context(), admission.Child.ID)
			require.ErrorIs(t, err, ErrNotFound)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, cs.AdmitChild(ctx, admission), context.Canceled)
			require.False(t, IsTemporary(context.Canceled))
		})
	}
}

func TestCoordinationDeepHistoryUsesCurrentOwnerRows(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			cs := store.(CoordinationStore)
			child := coordinationAdmission(root.ID)
			require.NoError(t, cs.AdmitChild(t.Context(), child))
			grandchild := coordinationAdmission(child.Child.ID)
			grandchild.Record.RootSessionID = root.ID
			grandchild.Record.Node.ID = "grandchild"
			require.NoError(t, cs.AdmitChild(t.Context(), grandchild))
			loaded, err := store.GetSession(t.Context(), grandchild.Child.ID)
			require.NoError(t, err)
			loaded.SetTitle("new metadata")
			require.NoError(t, store.UpdateSession(t.Context(), loaded))
			id, err := store.AddMessage(t.Context(), loaded.ID, UserMessage("new result"))
			require.NoError(t, err)
			// Stale parent and child snapshots can only reassert links.
			require.NoError(t, store.AddSubSession(t.Context(), root.ID, child.Child))
			require.NoError(t, store.AddSubSession(t.Context(), child.Child.ID, grandchild.Child))
			tree, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			nested := tree.Messages[0].SubSession.Messages[1].SubSession
			assert.Equal(t, "new metadata", nested.Title)
			require.Len(t, nested.Messages, 2)
			assert.Equal(t, id, nested.Messages[1].Message.ID)
			assert.Equal(t, "new result", nested.Messages[1].Message.Message.Content)
			nested.Messages[1].Message.Message.Content = "mutated snapshot"
			again, err := store.GetSession(t.Context(), nested.ID)
			require.NoError(t, err)
			assert.Equal(t, "new result", again.Messages[1].Message.Message.Content)
		})
	}
}
