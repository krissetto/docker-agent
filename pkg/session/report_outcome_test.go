package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReportOutcomePendingAndAcceptedSurviveRestart(t *testing.T) {
	for _, outcome := range []ReportOutcome{"", ReportOutcomeFinished, ReportOutcomeFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			root := New()
			require.NoError(t, store.AddSession(t.Context(), root))
			child := coordinationAdmission(root.ID)
			coord := store.(CoordinationStore)
			require.NoError(t, coord.AdmitChild(t.Context(), child))
			report := ChildReport{ID: "report", ParentSessionID: root.ID, ChildSessionID: child.Child.ID, TurnID: "child-turn", Content: "body", ReportOutcome: outcome}
			require.NoError(t, coord.CommitChild(t.Context(), ChildCommit{ExpectedRevision: 1, Record: child.Record, Reports: []ChildReport{report}}))
			require.NoError(t, store.Close())
			store, err = newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			coord = store.(CoordinationStore)
			pending, err := coord.PendingReports(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Equal(t, outcome, pending[0].ReportOutcome)
			input := UserMessage(report.Content)
			input.InputOrigin, input.SenderID, input.ReportOutcome, input.TurnID = InputOriginRuntime, child.Child.ID, pending[0].ReportOutcome, "report:report"
			_, err = coord.AcceptReport(t.Context(), root.ID, report.ID, input)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			defer store.Close()
			loaded, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			require.Equal(t, outcome, loaded.Messages[len(loaded.Messages)-1].Message.ReportOutcome)
			accepted, err := store.(CoordinationStore).AcceptReport(t.Context(), root.ID, report.ID, nil)
			require.NoError(t, err)
			require.Equal(t, outcome, accepted.Message.ReportOutcome)
		})
	}
}
