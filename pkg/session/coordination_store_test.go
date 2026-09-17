package session

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// Batch fixtures persist each owner separately, deliberately without parent
// links or coordination records, as in a legacy tree awaiting confirmation.
func coordinationBatchFixture(t *testing.T, store Store) (*Session, []ChildAdmission) {
	t.Helper()
	root := New()
	require.NoError(t, store.AddSession(t.Context(), root))
	admissions := make([]ChildAdmission, 3)
	for i := range admissions {
		a := coordinationAdmission(root.ID)
		a.Record.Node.ID = subagent.NodeID(fmt.Sprintf("batch-%d", i))
		a.Record.Node.Agent = "worker"
		a.Record.Revision = 1
		a.Child.SetAttribute("docker-agent.actor.agent", "worker")
		a.Child.AgentModelOverrides = map[string]string{"worker": "original-model"}
		require.NoError(t, store.AddSession(t.Context(), a.Child))
		a.Child = a.Child.OwnSnapshot()
		a.Child.Messages = nil
		admissions[i] = a
	}
	return root, admissions
}

func coordinationBatchStores(t *testing.T, test func(*testing.T, Store)) {
	t.Helper()
	for _, name := range []string{"memory", "sqlite"} {
		t.Run(name, func(t *testing.T) {
			var store Store
			if name == "memory" {
				store = NewInMemorySessionStore()
			} else {
				var err error
				store, err = newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "batch.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
			}
			test(t, store)
		})
	}
}

func assertCoordinationBatchUnadmitted(t *testing.T, store Store, root string, admissions []ChildAdmission) {
	t.Helper()
	records, err := store.(CoordinationStore).LoadChildren(t.Context(), root)
	require.NoError(t, err)
	require.Empty(t, records)
	parent, err := store.GetSession(t.Context(), root)
	require.NoError(t, err)
	require.Empty(t, parent.Messages, "failed batch must not append parent links")
	for _, a := range admissions {
		child, err := store.GetSession(t.Context(), a.Child.ID)
		require.NoError(t, err)
		require.Len(t, child.Messages, 1)
		assert.Equal(t, "task", child.Messages[0].Message.Message.Content)
	}
}

func TestCoordinationBatchValidationIsAtomic(t *testing.T) {
	t.Parallel()
	for _, invalid := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("invalid-%d", invalid), func(t *testing.T) {
			coordinationBatchStores(t, func(t *testing.T, store Store) {
				t.Helper()
				root, admissions := coordinationBatchFixture(t, store)
				// This is a stored adoption conflict, not just invalid input.
				bad := append([]ChildAdmission(nil), admissions...)
				bad[invalid].Child = admissions[invalid].Child.OwnSnapshot()
				bad[invalid].Child.Origin = "wrong-source"
				require.ErrorIs(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), bad), ErrAlreadyExists)
				assertCoordinationBatchUnadmitted(t, store, root.ID, admissions)
			})
		})
	}
}

func TestCoordinationBatchDuplicateAndAncestryConflicts(t *testing.T) {
	t.Parallel()
	for _, conflict := range []string{"session", "node", "parent", "ancestry", "binding", "record"} {
		t.Run(conflict, func(t *testing.T) {
			coordinationBatchStores(t, func(t *testing.T, store Store) {
				t.Helper()
				root, admissions := coordinationBatchFixture(t, store)
				bad := append([]ChildAdmission(nil), admissions...)
				switch conflict {
				case "session":
					bad[2] = bad[0]
				case "node":
					bad[2].Record.Node.ID = bad[0].Record.Node.ID
				case "parent":
					bad[2].Record.ParentSessionID = "missing"
				case "ancestry":
					other := New()
					require.NoError(t, store.AddSession(t.Context(), other))
					bad[2].Record.RootSessionID = other.ID
				case "binding":
					bad[2].Record.Node.Agent = "other-agent"
				case "record":
					old := admissions[2]
					old.Record.Node.ID = "previous-owner"
					require.NoError(t, store.(CoordinationStore).AdmitChild(t.Context(), old))
				}
				require.Error(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), bad))
				if conflict != "record" {
					assertCoordinationBatchUnadmitted(t, store, root.ID, admissions)
				} else {
					records, err := store.(CoordinationStore).LoadChildren(t.Context(), root.ID)
					require.NoError(t, err)
					require.Len(t, records, 1)
					assert.Equal(t, subagent.NodeID("previous-owner"), records[0].Node.ID)
					parent, err := store.GetSession(t.Context(), root.ID)
					require.NoError(t, err)
					require.Len(t, parent.Messages, 1)
				}
			})
		})
	}
}

func TestCoordinationBatchPreservesTranscriptsReceiptsAndAdvancedRecords(t *testing.T) {
	t.Parallel()
	coordinationBatchStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, admissions := coordinationBatchFixture(t, store)
		item := Item{Message: UserMessage("retained receipt")}
		id, err := store.(ItemAppender).AppendItem(t.Context(), admissions[0].Child.ID, "batch-receipt", item)
		require.NoError(t, err)
		before := make([]*Session, len(admissions))
		for i, a := range admissions {
			before[i], err = store.GetSession(t.Context(), a.Child.ID)
			require.NoError(t, err)
			// Incoming stale settings must never replace the stored binding.
			a.Child.AgentModelOverrides["worker"] = "stale-model"
		}
		batch := store.(ChildAdmissionBatchStore)
		require.NoError(t, batch.AdmitChildren(t.Context(), admissions))
		cs := store.(CoordinationStore)
		records, err := cs.LoadChildren(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, records, 3)
		for _, record := range records {
			assert.Equal(t, uint64(1), record.Revision)
		}
		record := records[0]
		record.LastTurnID, record.Result = "completed-turn", "retained result"
		report := ChildReport{ID: "batch-report", ParentSessionID: root.ID, ChildSessionID: record.Node.SessionID, TurnID: "completed-turn", Content: "retained report"}
		require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: record, Reports: []ChildReport{report}}))
		require.NoError(t, batch.AdmitChildren(t.Context(), admissions))
		for i, a := range admissions {
			require.NoError(t, cs.AdmitChild(t.Context(), a))
			after, err := store.GetSession(t.Context(), a.Child.ID)
			require.NoError(t, err)
			assert.Equal(t, before[i].Messages, after.Messages)
			assert.Equal(t, before[i].AgentModelOverrides, after.AgentModelOverrides)
			assert.Equal(t, before[i].AttributesSnapshot(), after.AttributesSnapshot())
		}
		retryID, err := store.(ItemAppender).AppendItem(t.Context(), admissions[0].Child.ID, "batch-receipt", item)
		require.NoError(t, err)
		assert.Equal(t, id, retryID)
		pending, err := cs.PendingReports(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, "retained report", pending[0].Content)
		records, err = cs.LoadChildren(t.Context(), root.ID)
		require.NoError(t, err)
		for _, current := range records {
			if current.Node.SessionID == record.Node.SessionID {
				assert.Equal(t, uint64(2), current.Revision)
				assert.Equal(t, "retained result", current.Result)
			}
		}
		parent, err := store.GetSession(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, parent.Messages, 3, "repeat confirmation must not duplicate links")
	})
}

func TestSQLiteCoordinationBatchWriteFailureRollsBack(t *testing.T) {
	t.Parallel()
	for _, invalid := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("insert-%d", invalid), func(t *testing.T) {
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "failure.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			root, admissions := coordinationBatchFixture(t, store)
			db := store.(*SQLiteSessionStore).db
			_, err = db.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_batch BEFORE INSERT ON child_records
WHEN json_extract(NEW.record, '$.Node.id') = 'batch-%d'
BEGIN SELECT RAISE(ABORT, 'injected batch failure'); END`, invalid))
			require.NoError(t, err)
			require.ErrorContains(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions), "injected batch failure")
			assertCoordinationBatchUnadmitted(t, store, root.ID, admissions)
			_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_batch`)
			require.NoError(t, err)
			require.NoError(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions))
		})
	}
}

func TestCoordinationBatchCancellationAndMissingRows(t *testing.T) {
	t.Parallel()
	coordinationBatchStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, admissions := coordinationBatchFixture(t, store)
		batch := store.(ChildAdmissionBatchStore)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.NoError(t, batch.AdmitChildren(ctx, nil), "empty batch is a no-op")
		require.ErrorIs(t, batch.AdmitChildren(ctx, admissions), context.Canceled)
		assertCoordinationBatchUnadmitted(t, store, root.ID, admissions)
		require.NoError(t, store.DeleteSession(t.Context(), admissions[1].Child.ID))
		require.ErrorIs(t, batch.AdmitChildren(t.Context(), admissions), ErrNotFound)
		_, err := store.GetSession(t.Context(), admissions[1].Child.ID)
		require.ErrorIs(t, err, ErrNotFound, "restore must not resurrect a deleted child")
		records, err := store.(CoordinationStore).LoadChildren(t.Context(), root.ID)
		require.NoError(t, err)
		require.Empty(t, records)
		parent, err := store.GetSession(t.Context(), root.ID)
		require.NoError(t, err)
		require.Empty(t, parent.Messages)
		spawn := coordinationAdmission(root.ID)
		spawn.Record.Revision = 1 // Ordinary spawn already uses revision one.
		require.ErrorIs(t, batch.AdmitChildren(t.Context(), []ChildAdmission{spawn}), ErrNotFound)
		require.NoError(t, store.(CoordinationStore).AdmitChild(t.Context(), spawn))
	})
}

func TestCoordinationConcurrentBatchesAndSingleAdmission(t *testing.T) {
	t.Parallel()
	coordinationBatchStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, admissions := coordinationBatchFixture(t, store)
		start := make(chan struct{})
		results := make(chan error, 3)
		for i := range 3 {
			go func() {
				<-start
				if i == 2 {
					results <- store.(CoordinationStore).AdmitChild(t.Context(), admissions[1])
				} else {
					results <- store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions)
				}
			}()
		}
		close(start)
		for range 3 {
			require.NoError(t, <-results)
		}
		records, err := store.(CoordinationStore).LoadChildren(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, records, 3)
		parent, err := store.GetSession(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, parent.Messages, 3)
	})
}

func TestCoordinationConcurrentDeleteDoesNotResurrect(t *testing.T) {
	t.Parallel()
	coordinationBatchStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, admissions := coordinationBatchFixture(t, store)
		start := make(chan struct{})
		admitted, deleted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			admitted <- store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions)
		}()
		go func() {
			<-start
			deleted <- store.DeleteSession(t.Context(), root.ID)
		}()
		close(start)
		require.NoError(t, <-deleted)
		err := <-admitted
		if err != nil {
			require.ErrorIs(t, err, ErrNotFound)
		}
		_, err = store.GetSession(t.Context(), root.ID)
		require.ErrorIs(t, err, ErrNotFound)
		records, err := store.(CoordinationStore).LoadChildren(t.Context(), root.ID)
		require.NoError(t, err)
		require.Empty(t, records)
		require.ErrorIs(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions), ErrNotFound)
	})
}

func TestCoordinationBatchNestedLegacyAncestry(t *testing.T) {
	t.Parallel()
	coordinationBatchStores(t, func(t *testing.T, store Store) {
		t.Helper()
		root, admissions := coordinationBatchFixture(t, store)
		parent := admissions[0].Child.ID
		child, err := store.GetSession(t.Context(), admissions[2].Child.ID)
		require.NoError(t, err)
		child.ParentID = parent
		require.NoError(t, store.UpdateSession(t.Context(), child))
		admissions[2].Child = child.OwnSnapshot()
		admissions[2].Child.Messages = nil
		admissions[2].Record.ParentSessionID = parent
		admissions[2].Record.Node.Parent = admissions[0].Record.Node.ID
		// Validate against stored owner rows, not input order or newly added links.
		admissions[0], admissions[2] = admissions[2], admissions[0]
		require.NoError(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions))
		loaded, err := store.GetSession(t.Context(), root.ID)
		require.NoError(t, err)
		require.Len(t, loaded.Messages, 2)
		loaded, err = store.GetSession(t.Context(), parent)
		require.NoError(t, err)
		require.Len(t, loaded.Messages, 2)
		assert.Equal(t, child.ID, loaded.Messages[1].SubSession.ID)
	})
}

type coordinationBatchStartedContext struct {
	getContext func() context.Context
	once       sync.Once
	started    chan struct{}
}

func (c *coordinationBatchStartedContext) Deadline() (time.Time, bool) {
	return c.getContext().Deadline()
}

func (c *coordinationBatchStartedContext) Done() <-chan struct{} {
	return c.getContext().Done()
}

func (c *coordinationBatchStartedContext) Err() error {
	c.once.Do(func() { close(c.started) })
	return c.getContext().Err()
}

func (c *coordinationBatchStartedContext) Value(key any) any {
	return c.getContext().Value(key)
}

func TestMemoryCoordinationBatchCanceledBeforeApply(t *testing.T) {
	t.Parallel()
	store := NewInMemorySessionStore().(*InMemorySessionStore)
	root, admissions := coordinationBatchFixture(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := &coordinationBatchStartedContext{getContext: func() context.Context { return ctx }, started: make(chan struct{})}
	store.coordinationMu.Lock()
	result := make(chan error, 1)
	go func() { result <- store.AdmitChildren(started, admissions) }()
	<-started.started
	cancel()
	store.coordinationMu.Unlock()
	require.ErrorIs(t, <-result, context.Canceled)
	assertCoordinationBatchUnadmitted(t, store, root.ID, admissions)
}

func TestSQLiteCoordinationBatchDeletedSnapshotCannotWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "delete-snapshot.db")
	// This test deliberately exercises a stale deferred snapshot, not the
	// production store's immediate writer-admission policy.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	sqlite, err := NewSQLiteSessionStoreFromDB(t.Context(), db)
	require.NoError(t, err)
	var store Store = sqlite
	var mode string
	require.NoError(t, sqlite.db.QueryRowContext(t.Context(), `PRAGMA journal_mode=WAL`).Scan(&mode))
	require.Equal(t, "wal", mode)
	otherDB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	otherDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, otherDB.Close()) })
	other, err := NewSQLiteSessionStoreFromDB(t.Context(), otherDB)
	require.NoError(t, err)
	root, admissions := coordinationBatchFixture(t, store)
	prepared, err := prepareAdmissions(admissions)
	require.NoError(t, err)
	// Force precisely the SQLite read-snapshot/delete/write ordering used by
	// admitChildren without adding a production transaction hook or lock.
	tx, err := sqlite.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var exists int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT 1 FROM sessions WHERE id = ?`, admissions[0].Child.ID).Scan(&exists))
	require.NoError(t, other.DeleteSession(t.Context(), admissions[0].Child.ID))
	err = sqlite.admitChildTx(t.Context(), tx, prepared[0])
	require.Error(t, err)
	require.True(t, IsTemporary(classifySQLiteError(err)), "stale snapshot must fail with a SQLite busy conflict: %v", err)
	require.NoError(t, tx.Rollback())
	_, err = store.GetSession(t.Context(), admissions[0].Child.ID)
	require.ErrorIs(t, err, ErrNotFound)
	records, err := store.(CoordinationStore).LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	require.Empty(t, records)
	parent, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	require.Empty(t, parent.Messages)
	require.ErrorIs(t, store.(ChildAdmissionBatchStore).AdmitChildren(t.Context(), admissions), ErrNotFound)
}
