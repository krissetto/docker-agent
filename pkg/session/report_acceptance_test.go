package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportAcceptanceReturnsPersistedState(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			cs := store.(CoordinationStore)
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			admission := coordinationAdmission(root.ID)
			require.NoError(t, cs.AdmitChild(t.Context(), admission))
			report := ChildReport{ID: "opaque-legacy-revision-7", ParentSessionID: root.ID, ChildSessionID: admission.Child.ID, TurnID: "child-turn", Content: "original report", ReportOutcome: ReportOutcomeFailed}
			require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: admission.Record, Reports: []ChildReport{report}}))
			probe, err := cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
			require.NoError(t, err)
			assert.Equal(t, ReportAcceptance{}, probe)
			pending, err := cs.PendingReports(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, report.Content, pending[0].Content)
			assert.Equal(t, report.ReportOutcome, pending[0].ReportOutcome)
			_, err = cs.AcceptReport(t.Context(), "wrong-parent", report.ID, nil)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = cs.AcceptReport(t.Context(), root.ID, "missing", nil)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = cs.AcceptReport(t.Context(), root.ID, report.ID, UserMessage("missing turn"))
			require.Error(t, err)
			input := ImplicitUserMessage("<system_info>original report</system_info>")
			input.ReportOutcome = report.ReportOutcome
			input.InputOrigin, input.InputMode, input.TurnID = InputOriginRuntime, "steer", "report:"+report.ID
			input.SenderID, input.SenderName = admission.Child.ID, "maker"
			accepted, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
			require.NoError(t, err)
			require.True(t, accepted.Created)
			require.NotZero(t, accepted.MessageID)
			require.NotNil(t, accepted.Message)
			assert.Zero(t, input.ID)
			assert.False(t, input.Pending)
			assert.True(t, accepted.Message.Pending)
			assert.True(t, accepted.Message.Accepted)
			assert.Equal(t, accepted.MessageID, accepted.Message.ID)
			assert.Equal(t, InputOriginRuntime, accepted.Message.InputOrigin)
			assert.Equal(t, input.SenderID, accepted.Message.SenderID)
			assert.Equal(t, input.SenderName, accepted.Message.SenderName)
			assert.Equal(t, input.ReportOutcome, accepted.Message.ReportOutcome)
			accepted.Message.Message.Content = "caller mutation"
			retry, err := cs.AcceptReport(t.Context(), root.ID, report.ID, UserMessage("retry must not replace original"))
			require.NoError(t, err)
			assert.False(t, retry.Created)
			assert.Equal(t, accepted.MessageID, retry.MessageID)
			require.NotNil(t, retry.Message)
			assert.Equal(t, input.Message.Content, retry.Message.Message.Content)
			assert.True(t, retry.Message.Pending)
			require.NoError(t, store.PromotePendingUserMessage(t.Context(), root.ID, input.TurnID))
			probe, err = cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
			require.NoError(t, err)
			require.NotNil(t, probe.Message)
			assert.False(t, probe.Message.Pending)
			assert.False(t, probe.Created)
			assert.Equal(t, accepted.MessageID, probe.MessageID)
			updated := cloneMessage(probe.Message)
			updated.Message.Content = "current durable body"
			require.NoError(t, store.UpdateMessage(t.Context(), root.ID, accepted.MessageID, updated))
			probe, err = cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
			require.NoError(t, err)
			assert.Equal(t, updated, probe.Message)
			// Model compaction may hide an accepted item without removing its row.
			_, err = store.(ItemAppender).AppendItem(t.Context(), root.ID, "summary", Item{Summary: "compacted", FirstKeptEntry: 2})
			require.NoError(t, err)
			probe, err = cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
			require.NoError(t, err)
			require.NotNil(t, probe.Message)
			assert.False(t, probe.Message.Pending)
			// A removed row still leaves the acknowledgement identity authoritative.
			removeAcceptedReportItem(t, store, root.ID, accepted.MessageID)
			for _, retryInput := range []*Message{nil, input} {
				probe, err = cs.AcceptReport(t.Context(), root.ID, report.ID, retryInput)
				require.NoError(t, err)
				assert.Equal(t, ReportAcceptance{MessageID: accepted.MessageID}, probe)
			}
			pending, err = cs.PendingReports(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Empty(t, pending)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = cs.AcceptReport(ctx, root.ID, report.ID, nil)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func removeAcceptedReportItem(t *testing.T, store Store, parent string, messageID int64) {
	t.Helper()
	switch store := store.(type) {
	case *InMemorySessionStore:
		stored, ok := store.sessions.Load(parent)
		require.True(t, ok)
		stored.mu.Lock()
		defer stored.mu.Unlock()
		for i, item := range stored.Messages {
			if item.Message != nil && item.Message.ID == messageID {
				stored.Messages = append(stored.Messages[:i], stored.Messages[i+1:]...)
				return
			}
		}
		t.Fatal("accepted message missing")
	case *SQLiteSessionStore:
		_, err := store.db.ExecContext(t.Context(), `DELETE FROM session_items WHERE session_id = ? AND id = ?`, parent, messageID)
		require.NoError(t, err)
	default:
		t.Fatalf("unexpected store %T", store)
	}
}

func TestReportAcceptanceKeepsDistinctChildTurns(t *testing.T) {
	t.Parallel()
	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			cs := store.(CoordinationStore)
			first := coordinationAdmission(root.ID)
			second := coordinationAdmission(root.ID)
			second.Record.Node.ID = "second-child"
			require.NoError(t, cs.AdmitChild(t.Context(), first))
			require.NoError(t, cs.AdmitChild(t.Context(), second))
			reports := []ChildReport{
				{ID: "legacy:child-a:revision-8", ParentSessionID: root.ID, ChildSessionID: first.Child.ID, TurnID: "turn-a", Content: "same result"},
				{ID: "child-a:turn-b", ParentSessionID: root.ID, ChildSessionID: first.Child.ID, TurnID: "turn-b", Content: "same result"},
				{ID: "child-b:turn-a", ParentSessionID: root.ID, ChildSessionID: second.Child.ID, TurnID: "turn-a", Content: "same result"},
			}
			require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: first.Record, Reports: reports[:2]}))
			require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: second.Record, Reports: reports[2:]}))
			ids := map[int64]bool{}
			for _, report := range reports {
				input := ImplicitUserMessage(report.Content)
				input.TurnID, input.InputOrigin = "report:"+report.ID, InputOriginRuntime
				accepted, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
				require.NoError(t, err)
				require.True(t, accepted.Created)
				require.False(t, ids[accepted.MessageID])
				ids[accepted.MessageID] = true
				retry, err := cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
				require.NoError(t, err)
				assert.Equal(t, accepted.MessageID, retry.MessageID)
				assert.False(t, retry.Created)
			}
			loaded, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Len(t, loaded.Messages, 5)
		})
	}
}

func TestSQLiteReportAcceptanceAtomicFailureAndRecovery(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reports.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	root := New()
	require.NoError(t, store.AddSession(t.Context(), root))
	cs := store.(CoordinationStore)
	admission := coordinationAdmission(root.ID)
	require.NoError(t, cs.AdmitChild(t.Context(), admission))
	report := ChildReport{ID: "old-opaque-report", ParentSessionID: root.ID, ChildSessionID: admission.Child.ID, TurnID: "turn", Content: "retained until accepted", ReportOutcome: ReportOutcomeFailed}
	require.NoError(t, cs.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: admission.Record, Reports: []ChildReport{report}}))
	db := store.(*SQLiteSessionStore).db
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_report_ack BEFORE UPDATE ON child_reports BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)
	input := ImplicitUserMessage(report.Content)
	input.ReportOutcome = report.ReportOutcome
	input.InputOrigin, input.InputMode, input.TurnID = InputOriginRuntime, "steer", "report:"+report.ID
	accepted, err := cs.AcceptReport(t.Context(), root.ID, report.ID, input)
	require.ErrorContains(t, err, "injected failure")
	assert.Equal(t, ReportAcceptance{}, accepted)
	loaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Len(t, loaded.Messages, 1)
	pending, err := cs.PendingReports(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, report.Content, pending[0].Content)
	assert.Equal(t, report.ReportOutcome, pending[0].ReportOutcome)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER reject_report_ack`)
	require.NoError(t, err)
	accepted, err = cs.AcceptReport(t.Context(), root.ID, report.ID, input)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cs = store.(CoordinationStore)
	// Treat the original response as lost; the probe recovers the committed input.
	probe, err := cs.AcceptReport(t.Context(), root.ID, report.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, accepted.MessageID, probe.MessageID)
	assert.Equal(t, accepted.Message, probe.Message)
	assert.False(t, probe.Created)
	removeAcceptedReportItem(t, store, root.ID, probe.MessageID)
	require.NoError(t, store.Close())
	store, err = newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	probe, err = store.(CoordinationStore).AcceptReport(t.Context(), root.ID, report.ID, input)
	require.NoError(t, err)
	assert.Equal(t, ReportAcceptance{MessageID: accepted.MessageID}, probe)
}
