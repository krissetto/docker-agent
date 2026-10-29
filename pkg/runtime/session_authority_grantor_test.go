package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// setRecordedGrantor rewrites the child's persisted and live grantor provenance
// together, so the attach-time binding check still matches and only grant
// resolution sees the change.
func setRecordedGrantor(t *testing.T, p *attachProbe, name, id string) {
	t.Helper()
	rewriteChildGrantor(t, p.rt, p.store, p.childID, name, id)
}

// rewriteChildGrantor rewrites a resident child's stored and live grantor together.
func rewriteChildGrantor(t *testing.T, rt *LocalRuntime, store session.Store, childID, name, id string) {
	t.Helper()
	driver, ok := rt.sessionDrivers.Lookup(childID)
	require.True(t, ok, "child must be resident")
	driver.mu.Lock()
	driver.sess.SetAttribute(SessionParentAgentAttribute, name)
	driver.sess.SetAttribute(SessionParentAgentIDAttribute, id)
	driver.mu.Unlock()
	row, err := store.GetSession(t.Context(), childID)
	require.NoError(t, err)
	row.SetAttribute(SessionParentAgentAttribute, name)
	row.SetAttribute(SessionParentAgentIDAttribute, id)
	require.NoError(t, store.UpdateSession(t.Context(), row))
}

// A recorded grantor that no longer resolves must fail closed, even though the
// parent's active agent would still authorize the child.
func TestResidentUnresolvedRecordedGrantorFailsClosed(t *testing.T) {
	t.Parallel()
	// Root still declares librarian (sync only), so an active-parent fallback would authorize.
	cases := []struct{ name, grantor, grantorID string }{
		{name: "missing id with live name", grantor: "root", grantorID: "missing-grantor-id"},
		{name: "unresolved name without id", grantor: "ghost-grantor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newAttachProbe(t, false)
			p.transfer(t)
			setRecordedGrantor(t, p, tc.grantor, tc.grantorID)
			_, _, err := p.service.LoadSession(t.Context(), p.childID)
			var sessionErr *SessionError
			require.ErrorAs(t, err, &sessionErr)
			require.Equal(t, SessionErrorConflict, sessionErr.Kind)
			require.Equal(t, SessionOperation("restore_tree_membership"), sessionErr.Operation)
		})
	}
}

// Absent provenance keeps the legacy active-parent grant.
func TestResidentAbsentRecordedGrantorUsesActiveParent(t *testing.T) {
	t.Parallel()
	p := newAttachProbe(t, false)
	p.transfer(t)
	setRecordedGrantor(t, p, "", "")
	_, snapshot, err := p.service.LoadSession(t.Context(), p.childID)
	require.NoError(t, err)
	require.Equal(t, p.childID, snapshot.ID)
	require.Equal(t, p.parentID, snapshot.ParentID)
}

// Persisted and live grantor IDs must agree; equal names alone do not authorize a restore.
func TestResidentGrantorIDDriftWithEqualNamesFailsClosed(t *testing.T) {
	t.Parallel()
	p := newAttachProbe(t, false)
	p.transfer(t)
	driver, ok := p.rt.sessionDrivers.Lookup(p.childID)
	require.True(t, ok)
	driver.mu.Lock()
	driver.sess.SetAttribute(SessionParentAgentIDAttribute, "drifted-grantor-id")
	driver.mu.Unlock()
	_, _, err := p.service.LoadSession(t.Context(), p.childID)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorConflict, sessionErr.Kind)
	require.Equal(t, SessionOperation("restore_binding"), sessionErr.Operation)
}
