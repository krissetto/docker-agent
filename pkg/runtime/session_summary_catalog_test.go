package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

type metadataOnlyCatalogStore struct {
	session.Store
	session.ScopedSummaryStore

	reads atomic.Int32
}

func (s *metadataOnlyCatalogStore) GetSessions(context.Context) ([]*session.Session, error) {
	return nil, errors.New("catalog must not hydrate sessions")
}

func (s *metadataOnlyCatalogStore) GetSession(context.Context, string) (*session.Session, error) {
	return nil, errors.New("catalog must not perform per-ID reads")
}

func (s *metadataOnlyCatalogStore) GetSessionSummariesWithScope(ctx context.Context, scope session.SummaryScope) ([]session.Summary, error) {
	s.reads.Add(1)
	return s.ScopedSummaryStore.GetSessionSummariesWithScope(ctx, scope)
}

func TestSessionSummaryCatalogMetadataOnlyScopeAndDeferredChild(t *testing.T) {
	base := session.NewInMemorySessionStore()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}), session.WithUserMessage("not a summary"))
	root.CreatedAt = time.Unix(100, 0).UTC()
	child := session.New(session.WithID("child"), session.WithParentID(root.ID), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker"}), session.WithUserMessage("not a preview"))
	child.CreatedAt = time.Unix(200, 0).UTC()
	child.AgentModelOverrides = map[string]string{"worker": "provider/model"}
	require.NoError(t, base.AddSession(t.Context(), root))
	require.NoError(t, base.AddSession(t.Context(), child))
	store := &metadataOnlyCatalogStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	catalog := owner.Runtime().(SessionSummaryCatalog)
	rows, err := catalog.ListSessionSummaries(t.Context(), SessionSummaryOptions{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, root.ID, rows[0].SessionID)
	assert.True(t, rows[0].Loadable)
	rows, err = catalog.ListSessionSummaries(t.Context(), SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, child.ID, rows[0].SessionID)
	assert.Equal(t, root.ID, rows[0].ParentID)
	assert.Equal(t, "provider/model", rows[0].Model)
	assert.False(t, rows[0].Loaded)
	assert.False(t, rows[0].Loadable)
	assert.True(t, rows[0].RequiresConfirmation, "cold child is a candidate, not a durable access grant")
	assert.EqualValues(t, 2, store.reads.Load(), "one metadata query per listing")
	_, err = owner.Runtime().SessionByID(child.ID)
	require.Error(t, err, "browsing never publishes a driver")
}

func TestSessionSummaryCatalogReusesLiveBindingWithoutHydration(t *testing.T) {
	base := session.NewInMemorySessionStore()
	rt, owner := coordinationRuntime(t, base, coordinationReply("root"), coordinationReply("child"))
	handle := coordinationCreate(t, owner.Runtime(), "live", "")
	driver := handle.(*sessionHandle).driver
	driver.SetModelBinding("live/provider-model", nil)
	store := &metadataOnlyCatalogStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}
	rt.sessionStore = store
	rows, err := owner.Runtime().(SessionSummaryCatalog).ListSessionSummaries(t.Context(), SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Loaded)
	assert.True(t, rows[0].Loadable)
	assert.Equal(t, "live/provider-model", rows[0].Model)
	assert.Equal(t, "root", rows[0].AgentName)
	assert.EqualValues(t, 1, store.reads.Load())
}

type fixedSummaryStore struct {
	session.Store

	rows []session.Summary
}

func (s fixedSummaryStore) GetSessionSummariesWithScope(context.Context, session.SummaryScope) ([]session.Summary, error) {
	return s.rows, nil
}

func TestSessionSummaryCatalogRejectsInvalidMetadataWithoutLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rows      []session.Summary
		wantError bool
	}{
		{name: "duplicate", rows: []session.Summary{{ID: "same"}, {ID: "same"}}, wantError: true},
		{name: "missing-id", rows: []session.Summary{{}}, wantError: true},
		{name: "missing-parent", rows: []session.Summary{{ID: "child", ParentID: "missing", Attributes: map[string]string{SessionAgentAttribute: "worker"}}}},
		{name: "cycle", rows: []session.Summary{{ID: "child", ParentID: "child", Attributes: map[string]string{SessionAgentAttribute: "worker"}}}},
		{name: "missing-agent", rows: []session.Summary{{ID: "root"}}},
		{name: "stale-agent", rows: []session.Summary{{ID: "root", Attributes: map[string]string{SessionAgentAttribute: "removed"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := fixedSummaryStore{Store: session.NewInMemorySessionStore(), rows: tc.rows}
			_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
			rows, err := owner.Runtime().(SessionSummaryCatalog).ListSessionSummaries(t.Context(), SessionSummaryOptions{IncludeChildren: true})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			for _, row := range rows {
				assert.False(t, row.Loadable)
				assert.False(t, row.RequiresConfirmation)
				assert.NotEmpty(t, row.RouteError)
			}
		})
	}
}

func TestSessionSummaryCatalogLargeCatalogOneRead(t *testing.T) {
	base := session.NewInMemorySessionStore()
	for i := range 1000 {
		sess := session.New(session.WithID(fmt.Sprintf("root-%04d", i)), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
		sess.CreatedAt = time.Unix(int64(i), 0).UTC()
		require.NoError(t, base.AddSession(t.Context(), sess))
	}
	store := &metadataOnlyCatalogStore{Store: base, ScopedSummaryStore: base.(session.ScopedSummaryStore)}
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	rows, err := owner.Runtime().(SessionSummaryCatalog).ListSessionSummaries(t.Context(), SessionSummaryOptions{IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, rows, 1000)
	assert.Equal(t, "root-0999", rows[0].SessionID)
	assert.Equal(t, "root-0000", rows[len(rows)-1].SessionID)
	assert.EqualValues(t, 1, store.reads.Load())
}
