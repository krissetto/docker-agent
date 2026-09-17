package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func TestPaneCatalogMergesExactCanonicalSessionsWithoutHydration(t *testing.T) {
	root, store := coldPaneRoot(t)
	root.paneCatalogOwner = root.application
	root.paneCatalog = []runtime.SessionSummaryEntry{
		{SessionID: "profile", Title: "persisted profile", Loadable: true},
		{SessionID: "persisted-cold", Title: "persisted cold", RequiresConfirmation: true},
		{SessionID: "closed-child", ParentID: "profile", Title: "Closed child", RequiresConfirmation: true},
		{SessionID: "closed-child", Title: "duplicate metadata", Loadable: true},
		{SessionID: "inaccessible", Title: "Missing source", RouteError: "source unavailable"},
	}
	rows := root.paneCatalogRows()
	byID := make(map[string]paneCatalogRow)
	for _, row := range rows {
		_, duplicate := byID[row.SessionID]
		require.False(t, duplicate)
		byID[row.SessionID] = row
	}
	require.Equal(t, "profile", byID["profile"].RoutingID)
	require.Equal(t, "cold", byID["persisted-cold"].RoutingID, "pending restored tab deduplicates by persisted canonical ID")
	require.True(t, paneCatalogSelectable(byID["closed-child"]), "confirmation is selectable metadata, not an access grant")
	require.False(t, paneCatalogSelectable(byID["inaccessible"]))
	require.Zero(t, store.reads.Load(), "catalog merge never reads transcript bodies")
	require.Nil(t, root.chatPages["cold"])
}

func TestPaneCatalogIgnoresForeignAndCanceledResult(t *testing.T) {
	root := splitTestRoot(t)
	root.paneCatalogOwner = root.application
	root.paneCatalog = []runtime.SessionSummaryEntry{{SessionID: "kept", Title: "original"}}
	generation, _ := root.supervisor.RouteGeneration("profile")
	canceled := false
	request := &paneCatalogRequest{application: root.application, owner: "profile", generation: generation, cancel: func() { canceled = true }}
	root.paneCatalogRequest = request
	root.cancelPaneCatalog()
	require.True(t, canceled)
	root.finishPaneCatalog(paneCatalogResult{request: request, entries: []runtime.SessionSummaryEntry{{SessionID: "late"}}})
	require.Equal(t, "kept", root.paneCatalog[0].SessionID)
	root.paneCatalogRequest = request
	root.handleSwitchTab("second")
	root.finishPaneCatalog(paneCatalogResult{request: request, entries: []runtime.SessionSummaryEntry{{SessionID: "foreign"}}})
	require.Equal(t, "kept", root.paneCatalog[0].SessionID)
}

func TestPaneCatalogCompletionIncludesConfirmedColdMetadata(t *testing.T) {
	root := splitTestRoot(t)
	root.paneCatalogOwner = root.application
	root.paneCatalog = []runtime.SessionSummaryEntry{{SessionID: "closed-full-identity", Title: "Archived notes", AgentName: "archivist", RequiresConfirmation: true}}
	candidates := root.paneArgumentCandidatesFor("right Archived")
	require.Len(t, candidates, 1)
	require.Equal(t, "right \"Archived notes\"", candidates[0].Value)
	require.NotContains(t, candidates[0].Label, "closed-full-identity")
	row, err := root.resolveCatalogSource("\"Archived notes\"")
	require.NoError(t, err)
	require.Equal(t, "closed-full-identity", row.SessionID)
	require.True(t, paneCatalogSelectable(row))
}
